package registry

import (
	stdtar "archive/tar"
	stdzip "archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/pivotal/kpack/pkg/source"
)

// SnapshotFetcher resolves a source image to a pinned digest and materializes
// every layer in memory. The pinned revision is the image's config digest as
// reported by the registry; a tag re-pointing between two reads cannot change
// the content under a pinned digest.
type SnapshotFetcher struct {
	Keychain authn.Keychain
	// Client overrides the registry client. It exists so tests can supply an
	// in-memory registry; production callers leave it nil.
	Client imageDigestClient
}

// NewSnapshotFetcher builds a fetcher whose keychain is resolved for a specific
// namespace and service account.
func NewSnapshotFetcher(keychain authn.Keychain) *SnapshotFetcher {
	return &SnapshotFetcher{Keychain: keychain}
}

type imageDigestClient interface {
	Fetch(keychain authn.Keychain, repoName string) (v1.Image, string, error)
}

// Discover implements source.DiscoveringFetcher. It resolves the image, derives
// the revision from its content digest, and returns the complete snapshot.
func (f *SnapshotFetcher) Discover(ctx context.Context, identity source.Identity) (*source.Snapshot, error) {
	img, identifier, err := f.fetchImage(ctx, identity.Location)
	if err != nil {
		return nil, err
	}

	digest, err := imageDigest(img, identifier)
	if err != nil {
		return nil, err
	}
	revision, err := source.DigestRevision(digest)
	if err != nil {
		return nil, err
	}

	return f.snapshotFromImage(identity, revision, img)
}

// Fetch implements source.Fetcher.
func (f *SnapshotFetcher) Fetch(ctx context.Context, pinned source.Pinned) (*source.Snapshot, error) {
	if pinned.Identity.Kind != source.KindRegistry {
		return nil, fmt.Errorf("registry snapshot fetcher cannot fetch kind %q", pinned.Identity.Kind)
	}

	img, identifier, err := f.fetchImage(ctx, pinned.Identity.Location)
	if err != nil {
		return nil, err
	}

	digest, err := imageDigest(img, identifier)
	if err != nil {
		return nil, err
	}
	if digest != pinned.Revision.Value {
		return nil, fmt.Errorf("source image %q resolved to digest %s, not the pinned digest %s", pinned.Identity.Location, digest, pinned.Revision.Value)
	}

	return f.snapshotFromImage(pinned.Identity, pinned.Revision, img)
}

func (f *SnapshotFetcher) snapshotFromImage(identity source.Identity, revision source.Revision, img v1.Image) (*source.Snapshot, error) {
	files, err := extractImageSource(img)
	if err != nil {
		return nil, err
	}

	snapshot := &source.Snapshot{
		Identity: identity,
		Revision: revision,
		Files:    map[string][]byte{},
	}
	for name, content := range files {
		entryType := source.EntryFile
		if content.isSymlink {
			entryType = source.EntrySymlink
		}
		snapshot.Entries = append(snapshot.Entries, source.Entry{
			Path: name,
			Type: entryType,
			Mode: content.mode,
			Hash: source.DigestFromBytes(content.data),
		})
		snapshot.Files[name] = content.data
	}
	sort.Slice(snapshot.Entries, func(i, j int) bool {
		return snapshot.Entries[i].Path < snapshot.Entries[j].Path
	})

	return snapshot, nil
}

func (f *SnapshotFetcher) fetchImage(_ context.Context, imageRef string) (v1.Image, string, error) {
	client := f.Client
	if client == nil {
		client = &Client{}
	}
	// The registry client is not context-aware today; it is a network call
	// bounded by the controller's resolve timeout.
	return client.Fetch(f.Keychain, imageRef)
}

func imageDigest(img v1.Image, identifier string) (string, error) {
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("reading image digest: %w", err)
	}

	// Prefer the digest embedded in the client identifier so the pinned value
	// matches what a later fetch would report.
	if parts := strings.SplitN(identifier, "@", 2); len(parts) == 2 && parts[1] != "" {
		return parts[1], nil
	}
	return digest.String(), nil
}

type imageFile struct {
	data      []byte
	mode      string
	isSymlink bool
}

// extractImageSource honors the source.contenttype.kpack.io label exactly like
// the historical fetcher: some source images carry a zip/tar archive in a
// single layer rather than a root filesystem of layers.
func extractImageSource(img v1.Image) (map[string]imageFile, error) {
	cType, err := getContentType(img)
	if err != nil {
		return nil, err
	}

	switch cType {
	case zip, jar, war:
		data, err := sourceFileBytes(img)
		if err != nil {
			return nil, err
		}
		return extractZipBytes(data, 0)
	case tar:
		data, err := sourceFileBytes(img)
		if err != nil {
			return nil, err
		}
		return extractTarBytes(bytes.NewReader(data), 0)
	case targz:
		data, err := sourceFileBytes(img)
		if err != nil {
			return nil, err
		}
		gzr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return extractTarBytes(gzr, 0)
	default:
		return extractImageLayers(img)
	}
}

func extractImageLayers(img v1.Image) (map[string]imageFile, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}

	files := map[string]imageFile{}
	for _, layer := range layers {
		reader, err := layer.Uncompressed()
		if err != nil {
			return nil, err
		}
		if err := readTarInto(files, reader); err != nil {
			reader.Close()
			return nil, err
		}
		reader.Close()
	}
	return files, nil
}

// sourceFileBytes returns the contents of the single file in a
// content-typed source image layer.
func sourceFileBytes(img v1.Image) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	if len(layers) != 1 {
		return nil, fmt.Errorf("expected source image to have exactly one layer")
	}

	reader, err := layers[0].Uncompressed()
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	files := map[string]imageFile{}
	if err := readTarInto(files, reader); err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("expected source image layer to contain exactly one file")
	}
	for _, file := range files {
		return file.data, nil
	}
	return nil, fmt.Errorf("expected source image layer to contain exactly one file")
}

func extractTarBytes(reader io.Reader, stripComponents int) (map[string]imageFile, error) {
	files := map[string]imageFile{}
	tarReader := stdtar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return files, nil
		} else if err != nil {
			return nil, err
		}

		name := stripComponentsFromPath(header.Name, stripComponents)
		if name == "" || header.Typeflag == stdtar.TypeDir {
			continue
		}
		switch header.Typeflag {
		case stdtar.TypeSymlink:
			files[name] = imageFile{data: []byte(header.Linkname), mode: "120000", isSymlink: true}
		case stdtar.TypeReg:
			content, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, err
			}
			files[name] = imageFile{data: content, mode: fmt.Sprintf("%o", header.Mode)}
		}
	}
}

func extractZipBytes(data []byte, stripComponents int) (map[string]imageFile, error) {
	reader, err := stdzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	files := map[string]imageFile{}
	for _, file := range reader.File {
		name := stripComponentsFromPath(file.Name, stripComponents)
		if name == "" || file.FileInfo().IsDir() {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(opened)
		opened.Close()
		if err != nil {
			return nil, err
		}
		entry := imageFile{data: content, mode: fmt.Sprintf("%o", file.Mode().Perm())}
		if file.Mode()&os.ModeSymlink != 0 {
			entry.isSymlink = true
			entry.mode = "120000"
		}
		files[name] = entry
	}
	return files, nil
}

func readTarInto(files map[string]imageFile, reader io.Reader) error {
	tarReader := stdtar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		name := cleanArchivePath(header.Name)
		if name == "" || header.Typeflag == stdtar.TypeDir {
			continue
		}

		switch header.Typeflag {
		case stdtar.TypeSymlink:
			files[name] = imageFile{data: []byte(header.Linkname), mode: "120000", isSymlink: true}
		case stdtar.TypeReg:
			content, err := io.ReadAll(tarReader)
			if err != nil {
				return err
			}
			files[name] = imageFile{data: content, mode: fmt.Sprintf("%o", header.Mode)}
		}
	}
}

// cleanArchivePath rejects archive members that would escape the extraction
// root rather than cleaning them into a silent alias.
func cleanArchivePath(name string) string {
	components := strings.Split(strings.ReplaceAll(name, `\`, "/"), "/")
	cleaned := path.Clean(strings.Join(components, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return ""
	}
	return cleaned
}

func stripComponentsFromPath(name string, stripComponents int) string {
	components := strings.Split(strings.ReplaceAll(name, `\`, "/"), "/")
	if len(components) <= stripComponents {
		return ""
	}
	return cleanArchivePath(strings.Join(components[stripComponents:], "/"))
}
