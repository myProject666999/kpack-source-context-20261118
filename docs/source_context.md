# Source context: subdirectory trimming and shared source caching

## Motivation

A monorepo often backs several `Image`s that share one repository and branch but
build different subdirectories. Without source-context support, every commit on
the branch fetches the whole repository and can trigger a rebuild of every
`Image`, even when only unrelated directories changed.

Source-context support gives each `Image` a **trimmed build context** and makes
the decision to rebuild depend on the content of that context rather than on the
whole revision. Full source for a revision is fetched once and shared.

## Configuration

```yaml
apiVersion: kpack.io/v1alpha2
kind: Image
spec:
  source:
    git:
      url: https://github.com/example/monorepo
      revision: main
    subPath: apps/a        # build this directory only
    excludes:              # remove these from the build context
    - "**/testdata"
    - "*.log"
```

`subPath` and `excludes` also work for `blob` and `registry` sources. Exclude
patterns are gitignore-style and are matched relative to the context root (the
`subPath`, or the source root when `subPath` is unset). A pattern starting with
`/` is anchored to the context root; it is not a filesystem-absolute path.

When neither `subPath` nor `excludes` is set, kpack behaves **exactly** as
before: no extra fetch, no pinned digest, no context digest, and byte-for-byte
identical build inputs. This is why introducing the feature does not cause a
cluster-wide rebuild.

## Pinned revisions

Every source is resolved to an immutable revision before a build starts:

| Source   | Pinned revision                          |
|----------|------------------------------------------|
| git      | commit hash                              |
| blob     | sha256 of the downloaded archive bytes   |
| registry | image manifest digest                    |

Once pinned, the build uses that revision. It does not re-resolve a branch name,
tag, URL or image tag. Even if a branch advances between two reads, every step of
one build receives the same content, and build-init verifies blob/image content
against the pinned digest before using it.

## Change detection: context digest vs. whole revision

There are two ways to decide whether a commit requires a rebuild:

* **Context digest (precise).** kpack computes a digest over only the files
  selected by `subPath`/`excludes`. A commit that touches only other
  directories leaves the digest unchanged and does **not** trigger a rebuild.
  This is precise, but it requires fetching the revision's content at decision
  time.
* **Whole-revision digest (cheap).** kpack compares the full revision and
  filters before building. Deciding is cheap because no content is needed, but
  unrelated commits still schedule builds that are then filtered or simply
  produce a redundant build.

kpack uses the context digest whenever trimming is configured. The full source
snapshot needed to compute it is cached and shared, so the extra cost is paid
once per revision rather than once per `Image`.

## Caching

Full source snapshots are keyed by `(kind, location, extraction variant,
revision)`. Derived contexts extend that key with the canonical
`(subPath, excludes)` parameters. Consequences:

* Two `Image`s that share a repository and revision but use different
  subdirectories fetch the source **once** and derive two contexts.
* Different extraction options (for example a blob's `strip-components`) never
  alias, and different trimming parameters never reuse each other's content.
* Writes are safe under concurrency: an in-flight entry is not visible until the
  fetch completes, so a failed or interrupted fetch is never read as a complete
  entry; the next request retries.
* Entries in use by an `Image` can be leased so they are not evicted out from
  under a running build.

## Boundary handling

The following inputs are rejected with a clear error rather than silently
producing a truncated or escaped build:

* `subPath` or an exclude pattern containing an up-level (`..`) component.
* Windows drive-letter (`C:\...`) and UNC (`\\server\share`) paths.
* Symlinks whose targets resolve outside the source root.
* A `subPath` that does not exist in the pinned revision (an explicit
  `SubPathNotFound` error, not an empty build).
* Exclude rules that remove every file (an explicit `EmptyContext` error).
* Paths that differ only by case, which would be ambiguous on a
  case-insensitive filesystem.

A leading `/` on `subPath` is treated as root-relative (kpack has historically
accepted values such as `/go`); it cannot escape the source root.

## Compatibility

Images **without** `subPath` and `excludes` are unaffected: their resolved
source, build inputs, pod specification, and source digest are unchanged. Images
that already used `subPath` keep building the same directory; they gain precise
change detection and a pinned revision. On upgrade, the first reconcile records
a context digest but does not schedule an extra build because the revision is
unchanged.

No migration is required. Removing `subPath` (and leaving `excludes` unset)
returns an Image to the historical whole-repository behavior on the next
revision change. Note that a change to the trimming parameters is a
configuration change and schedules one build, because the build context
itself changed.
