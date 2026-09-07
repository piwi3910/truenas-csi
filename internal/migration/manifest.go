package migration

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ErrVerification means the target does not provably hold the same data as the
// source. It is the one outcome a migration must never round up to success: a
// partial copy reported as complete is worse than a failed migration, because
// the operator will then delete the source.
var ErrVerification = errors.New("migration verification failed")

// maxNamed caps how many paths an error names, so a wholly-failed copy produces
// a diagnosis rather than a million-line log record.
const maxNamed = 10

// Manifest is a checksum manifest: one sha256 per regular file, keyed by the
// file's path relative to the root of the volume.
//
// It is produced by `find . -type f | sort | xargs sha256sum` inside the copy
// Job, on both sides, and is the evidence that the copy is complete.
type Manifest struct {
	Files map[string]string
}

// ParseManifest reads sha256sum output ("<hex>  <path>") into a Manifest.
func ParseManifest(r io.Reader) (Manifest, error) {
	m := Manifest{Files: map[string]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		sum, path, ok := strings.Cut(text, " ")
		if !ok {
			return Manifest{}, fmt.Errorf("manifest line %d: %q is not <sha256> <path>", line, text)
		}
		// sha256sum separates with two spaces, the second being the binary/text
		// mode flag position; a path may itself begin with a space, so only the
		// separator is trimmed, not the path.
		path = strings.TrimPrefix(path, " ")
		path = strings.TrimPrefix(path, "*")
		path = strings.TrimPrefix(path, "./")
		if sum == "" || path == "" {
			return Manifest{}, fmt.Errorf("manifest line %d: empty checksum or path", line)
		}
		m.Files[path] = sum
	}
	if err := sc.Err(); err != nil {
		return Manifest{}, fmt.Errorf("reading manifest: %w", err)
	}
	return m, nil
}

// Verify reports whether target holds exactly the source's files with exactly
// the source's contents.
//
// This is the reference implementation of the check the copy Job performs on
// the two mounted filesystems: same file count, same paths, same checksums. It
// is exported so an operator who has fetched both manifests can re-run the
// judgement themselves rather than trusting the Job's exit code.
func Verify(source, target Manifest) error {
	var missing, differing, extra []string
	for path, sum := range source.Files {
		got, ok := target.Files[path]
		switch {
		case !ok:
			missing = append(missing, path)
		case got != sum:
			differing = append(differing, path)
		}
	}
	for path := range target.Files {
		if _, ok := source.Files[path]; !ok {
			extra = append(extra, path)
		}
	}
	if len(missing) == 0 && len(differing) == 0 && len(extra) == 0 {
		if len(source.Files) != len(target.Files) {
			return fmt.Errorf("%w: source has %d files, target has %d",
				ErrVerification, len(source.Files), len(target.Files))
		}
		return nil
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("source has %d files, target has %d",
		len(source.Files), len(target.Files)))
	for _, g := range []struct {
		label string
		paths []string
	}{
		{"missing from target", missing},
		{"differing checksum", differing},
		{"unexpected in target", extra},
	} {
		if len(g.paths) == 0 {
			continue
		}
		sort.Strings(g.paths)
		shown := g.paths
		suffix := ""
		if len(shown) > maxNamed {
			shown, suffix = shown[:maxNamed], fmt.Sprintf(" (+%d more)", len(g.paths)-maxNamed)
		}
		parts = append(parts, fmt.Sprintf("%d %s: %s%s",
			len(g.paths), g.label, strings.Join(shown, ", "), suffix))
	}
	return fmt.Errorf("%w: %s", ErrVerification, strings.Join(parts, "; "))
}

// Summary is what the copy Job writes to its termination log once it has
// compared the two manifests itself. The driver has neither volume mounted, so
// this is the only evidence it can read back.
type Summary struct {
	SourceFiles  int64  `json:"source_files"`
	TargetFiles  int64  `json:"target_files"`
	SourceDigest string `json:"source_digest"`
	TargetDigest string `json:"target_digest"`
	BytesCopied  int64  `json:"bytes_copied"`
	Verified     bool   `json:"verified"`
}

// ParseSummary decodes a Summary from a container's termination message.
func ParseSummary(b []byte) (*Summary, error) {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil, nil
	}
	var s Summary
	if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
		return nil, fmt.Errorf("decoding migration summary %q: %w", trimmed, err)
	}
	return &s, nil
}

// Check reports whether the Job's own comparison proves the copy complete.
//
// A nil Summary is a failure, not a pass: a Job that exited zero without
// reporting a comparison has proved nothing, and defaulting that to success is
// exactly how a partial copy gets declared complete.
func (s *Summary) Check() error {
	if s == nil {
		return fmt.Errorf("%w: the copy job reported no verification summary", ErrVerification)
	}
	if s.SourceFiles != s.TargetFiles {
		return fmt.Errorf("%w: source has %d files, target has %d",
			ErrVerification, s.SourceFiles, s.TargetFiles)
	}
	if s.SourceDigest == "" || s.TargetDigest == "" {
		return fmt.Errorf("%w: the copy job reported no manifest digest", ErrVerification)
	}
	if s.SourceDigest != s.TargetDigest {
		return fmt.Errorf("%w: manifest digest %s != %s",
			ErrVerification, s.SourceDigest, s.TargetDigest)
	}
	if !s.Verified {
		return fmt.Errorf("%w: the copy job did not declare the copy verified", ErrVerification)
	}
	return nil
}
