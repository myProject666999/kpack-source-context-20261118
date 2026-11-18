package blob

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/pivotal/kpack/pkg/source"
)

// SnapshotFetcher downloads a blob archive and extracts it into memory for a
// fully materialized, revision-pinned snapshot. The pinned revision is the
// sha256 digest of the downloaded archive bytes; a mismatch is treated as an
// error so a moving URL can never silently change a pinned build.
type SnapshotFetcher struct {
	Logger   Logger
	Keychain Keychain
}

// Logger is the subset of *log.Logger used by the fetcher.
type Logger interface {
	Printf(format string, v ...interface{})
}

// Discover implements source.DiscoveringFetcher. It downloads the blob once,
// derives the revision from the archive digest, and returns the complete
// snapshot so the same download serves both discovery and materialization.
func (f *SnapshotFetcher) Discover(ctx context.Context, identity source.Identity) (*source.Snapshot, error) {
	headers, err := f.authHeaders(identity.Location)
	if err != nil {
		return nil, err
	}

	data, err := downloadBytes(ctx, identity.Location, headers)
	if err != nil {
		return nil, err
	}

	revision, err := source.DigestRevision(source.DigestFromBytes(data))
	if err != nil {
		return nil, err
	}

	return f.snapshotFromBytes(identity, revision, data)
}

// Fetch implements source.Fetcher.
func (f *SnapshotFetcher) Fetch(ctx context.Context, pinned source.Pinned) (*source.Snapshot, error) {
	if pinned.Identity.Kind != source.KindBlob {
		return nil, fmt.Errorf("blob snapshot fetcher cannot fetch kind %q", pinned.Identity.Kind)
	}

	headers, err := f.authHeaders(pinned.Identity.Location)
	if err != nil {
		return nil, err
	}

	data, err := downloadBytes(ctx, pinned.Identity.Location, headers)
	if err != nil {
		return nil, err
	}

	return f.snapshotFromBytes(pinned.Identity, pinned.Revision, data)
}

func (f *SnapshotFetcher) snapshotFromBytes(identity source.Identity, revision source.Revision, data []byte) (*source.Snapshot, error) {
	digest := source.DigestFromBytes(data)
	if digest != revision.Value {
		return nil, fmt.Errorf("blob %q content digest %s does not match pinned revision %s", identity.Location, digest, revision.Value)
	}

	stripComponents := stripComponentsFromVariant(identity.Variant)
	files, err := extractArchive(identity.Location, data, stripComponents)
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

func (f *SnapshotFetcher) authHeaders(location string) (map[string]string, error) {
	if f.Keychain == nil {
		return nil, nil
	}
	auth, headers, err := f.Keychain.Resolve(location)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve creds: %w", err)
	}
	if headers == nil {
		headers = make(map[string]string)
	}
	if auth != "" {
		headers["Authorization"] = auth
	}
	return headers, nil
}

type extractedFile struct {
	data      []byte
	mode      string
	isSymlink bool
}

func extractArchive(location string, data []byte, stripComponents int) (map[string]extractedFile, error) {
	// Detect the archive type from content, matching the historical fetcher.
	contentType := detectContentType(data)
	switch contentType {
	case "application/zip":
		return extractZipBytes(data, stripComponents)
	case "application/x-gzip":
		gzr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return extractTarBytes(gzr, stripComponents)
	default:
		if !isTarBytes(data) {
			return nil, errUnexpectedBlobType
		}
		return extractTarBytes(bytes.NewReader(data), stripComponents)
	}
}

func extractTarBytes(reader io.Reader, stripComponents int) (map[string]extractedFile, error) {
	files := map[string]extractedFile{}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		name := stripComponentsFromPath(header.Name, stripComponents)
		if name == "" {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink:
			files[name] = extractedFile{data: []byte(header.Linkname), mode: "120000", isSymlink: true}
		case tar.TypeReg:
			content, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, err
			}
			files[name] = extractedFile{data: content, mode: fmt.Sprintf("%o", header.Mode)}
		}
	}
	return files, nil
}

func extractZipBytes(data []byte, stripComponents int) (map[string]extractedFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	files := map[string]extractedFile{}
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

		mode := file.Mode()
		entry := extractedFile{data: content, mode: fmt.Sprintf("%o", mode.Perm())}
		if mode&os.ModeSymlink != 0 {
			entry.isSymlink = true
			entry.mode = "120000"
		}
		files[name] = entry
	}
	return files, nil
}

func stripComponentsFromPath(name string, stripComponents int) string {
	components := strings.Split(strings.ReplaceAll(name, `\`, "/"), "/")
	if len(components) <= stripComponents {
		return ""
	}
	cleaned := path.Clean(strings.Join(components[stripComponents:], "/"))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}

// stripComponentsFromVariant recovers the extraction option encoded in the
// source identity variant. Keeping it on the identity means differently
// extracted copies of one archive never alias in the cache.
func stripComponentsFromVariant(variant string) int {
	const prefix = "strip-components="
	if !strings.HasPrefix(variant, prefix) {
		return 0
	}
	var value int
	if _, err := fmt.Sscanf(strings.TrimPrefix(variant, prefix), "%d", &value); err != nil {
		return 0
	}
	return value
}

// StripComponentsVariant builds the identity variant for a strip-components
// value. A zero value produces an empty variant so existing sources keep their
// identity unchanged. The resolver and fetcher share this format.
func StripComponentsVariant(stripComponents int64) string {
	if stripComponents == 0 {
		return ""
	}
	return fmt.Sprintf("strip-components=%d", stripComponents)
}
