// Package intelligencepatch applies bounded, untrusted model proposals in an
// exclusively owned worker source snapshot. It grants no merge or publication
// authority. The caller must run its trusted checks and review the candidate.
package intelligencepatch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxFiles = 3
	// MaxBytes bounds the combined original and replacement text in Apply.
	MaxBytes = 32 * 1024
)

type Edit struct {
	Path       string `json:"path"`
	BeforeHash string `json:"before_hash"`
	Content    string `json:"content"`
}

type Proposal struct {
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	Edits   []Edit `json:"edits"`
}

var (
	gitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Validate checks the proposal without accessing a filesystem. allowedPaths
// must come from trusted worker configuration, never from the model response.
func Validate(p Proposal, baseSHA, headSHA string, allowedPaths []string) error {
	if !gitSHA.MatchString(baseSHA) || !gitSHA.MatchString(headSHA) || p.BaseSHA != baseSHA || p.HeadSHA != headSHA {
		return errors.New("patch source identity mismatch")
	}
	if len(p.Edits) == 0 || len(p.Edits) > MaxFiles || len(allowedPaths) == 0 || len(allowedPaths) > MaxFiles {
		return errors.New("patch file count is outside limits")
	}
	allowed := make(map[string]bool, len(allowedPaths))
	for _, name := range allowedPaths {
		if !safePath(name) || allowed[strings.ToLower(name)] {
			return errors.New("unsafe or duplicate patch allowlist path")
		}
		allowed[strings.ToLower(name)] = true
	}
	seen := make(map[string]bool, len(p.Edits))
	bytes := 0
	for _, edit := range p.Edits {
		if !safePath(edit.Path) || !containsExact(allowedPaths, edit.Path) || seen[strings.ToLower(edit.Path)] {
			return errors.New("unsafe, duplicate, or unapproved patch path")
		}
		seen[strings.ToLower(edit.Path)] = true
		if !digest.MatchString(edit.BeforeHash) {
			return errors.New("patch requires an exact SHA-256 preimage hash")
		}
		bytes += len(edit.Content)
		if bytes > MaxBytes || !text(edit.Content) || conflictMarkers(edit.Content) {
			return errors.New("patch replacement is not bounded resolved text")
		}
	}
	return nil
}

func containsExact(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func safePath(name string) bool {
	if name == "" || len(name) > 256 || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, c := range name {
		if c < 33 || c > 126 || strings.ContainsRune(":*?\"<>|", c) {
			return false
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) > 16 {
		return false
	}
	for _, part := range parts {
		part = strings.ToLower(part)
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") {
			return false
		}
		stem := strings.SplitN(part, ".", 2)[0]
		if stem == "con" || stem == "prn" || stem == "aux" || stem == "nul" || (len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) && stem[3] >= '0' && stem[3] <= '9') {
			return false
		}
		for _, word := range []string{"secret", "credential", "password", "migration", "token"} {
			if strings.Contains(part, word) {
				return false
			}
		}
		for _, prefix := range []string{"auth", "security", "crypto", "config"} {
			if strings.HasPrefix(part, prefix) {
				return false
			}
		}
		switch part {
		case "vendor", "node_modules", "deps", "dependencies", "go.mod", "go.sum", "makefile", "dockerfile", "gemfile", "gemfile.lock", "cargo.lock", "cargo.toml", "pyproject.toml", "poetry.lock", "uv.lock", "requirements.txt", "requirements-dev.txt":
			return false
		}
	}
	// Configuration, executable scripts, binary formats and dependency manifests
	// intentionally have no supported extension in this small-conflict path.
	switch strings.ToLower(path.Ext(name)) {
	case ".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".rs", ".java", ".kt", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".rb", ".php", ".swift", ".scala", ".vue", ".svelte", ".css", ".scss", ".html", ".md", ".txt":
		return true
	}
	return false
}

func text(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c == 0 || c == 127 || (c < 32 && c != '\n' && c != '\r' && c != '\t') {
			return false
		}
	}
	return true
}

func conflictMarkers(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, "|||||||") || strings.HasPrefix(line, ">>>>>>>") || line == "=======" {
			return true
		}
	}
	return false
}

type prepared struct {
	edit          Edit
	parent        *os.Root
	leaf          string
	before        []byte
	info          os.FileInfo
	stage, backup string
	applied       bool
}

// Apply stages all edits after validating every preimage, then replaces the
// existing files. It preserves permission bits, including read-only snapshots.
// An application error triggers rollback; this is not a crash-atomic multi-file
// transaction. The source snapshot must be exclusively owned by this worker.
func Apply(root string, p Proposal, baseSHA, headSHA string, allowedPaths []string) error {
	if err := Validate(p, baseSHA, headSHA, allowedPaths); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("patch root must be an existing nonsymlink directory")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("cannot open patch root")
	}
	defer r.Close()
	if actual, err := r.Stat("."); err != nil || !os.SameFile(rootInfo, actual) {
		return errors.New("patch root changed")
	}
	items := make([]*prepared, 0, len(p.Edits))
	complete := false
	defer func() {
		for _, item := range items {
			names := []string{item.stage}
			if complete || !item.applied {
				names = append(names, item.backup)
			}
			for _, name := range names {
				if name != "" {
					_ = item.parent.Remove(name)
				}
			}
			_ = item.parent.Close()
		}
	}()
	total := 0
	for _, edit := range p.Edits {
		parent, err := openParent(r, edit.Path)
		if err != nil {
			return err
		}
		item := &prepared{edit: edit, parent: parent, leaf: path.Base(edit.Path)}
		items = append(items, item)
		item.before, item.info, err = readOriginal(parent, item.leaf)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(item.before)
		if "sha256:"+hex.EncodeToString(sum[:]) != edit.BeforeHash {
			return errors.New("patch preimage hash mismatch")
		}
		total += len(item.before) + len(edit.Content)
		if total > MaxBytes {
			return errors.New("patch combined text exceeds limit")
		}
	}
	for _, item := range items {
		item.stage, err = stage(item.parent, []byte(item.edit.Content), item.info.Mode().Perm())
		if err != nil {
			return err
		}
		item.backup, err = stage(item.parent, item.before, item.info.Mode().Perm())
		if err != nil {
			return err
		}
	}
	// Recheck every preimage after staging and before the first replacement.
	for _, item := range items {
		if err := unchanged(item); err != nil {
			return err
		}
	}
	for _, item := range items {
		if err := unchanged(item); err != nil {
			return errors.Join(err, rollback(items))
		}
		if err := item.parent.Rename(item.stage, item.leaf); err != nil {
			return errors.Join(errors.New("cannot replace patch file"), rollback(items))
		}
		item.stage = ""
		item.applied = true
	}
	complete = true
	return nil
}

func openParent(root *os.Root, name string) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, errors.New("cannot open patch parent")
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, errors.New("patch parent must be an existing nonsymlink directory")
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			current.Close()
			return nil, errors.New("cannot open patch parent")
		}
		actual, statErr := next.Stat(".")
		check, checkErr := current.Lstat(part)
		current.Close()
		if statErr != nil || checkErr != nil || check.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, actual) || !os.SameFile(info, check) {
			next.Close()
			return nil, errors.New("patch parent changed")
		}
		current = next
	}
	return current, nil
}

func readOriginal(parent *os.Root, name string) ([]byte, os.FileInfo, error) {
	info, err := parent.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() > MaxBytes {
		return nil, nil, errors.New("patch target must be an existing bounded regular file")
	}
	f, err := openNoFollow(parent, name)
	if err != nil {
		return nil, nil, errors.New("cannot open nonsymlink patch file")
	}
	defer f.Close()
	actual, err := f.Stat()
	check, checkErr := parent.Lstat(name)
	if err != nil || checkErr != nil || !actual.Mode().IsRegular() || check.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, actual) || !os.SameFile(info, check) {
		return nil, nil, errors.New("patch target changed")
	}
	content, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(content) > MaxBytes || !text(string(content)) {
		return nil, nil, errors.New("patch preimage is not bounded text")
	}
	return content, actual, nil
}

func unchanged(item *prepared) error {
	content, info, err := readOriginal(item.parent, item.leaf)
	if err != nil || !os.SameFile(item.info, info) || info.Mode() != item.info.Mode() || string(content) != string(item.before) {
		return errors.New("patch preimage changed before replacement")
	}
	return nil
}

func stage(parent *os.Root, content []byte, mode os.FileMode) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", errors.New("cannot allocate patch stage")
	}
	name := ".rcp-intelligence-" + hex.EncodeToString(token[:])
	f, err := parent.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", errors.New("cannot create patch stage")
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			_ = parent.Remove(name)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return "", errors.New("cannot write patch stage")
	}
	if err := f.Chmod(mode); err != nil {
		return "", errors.New("cannot preserve patch file mode")
	}
	if err := f.Sync(); err != nil {
		return "", errors.New("cannot sync patch stage")
	}
	if err := f.Close(); err != nil {
		return "", errors.New("cannot close patch stage")
	}
	ok = true
	return name, nil
}

func rollback(items []*prepared) error {
	var result error
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if !item.applied {
			continue
		}
		current, _, err := readOriginal(item.parent, item.leaf)
		if err != nil || string(current) != item.edit.Content {
			result = errors.Join(result, fmt.Errorf("patch rollback refused changed target %q; original retained at %q", item.edit.Path, path.Join(path.Dir(item.edit.Path), item.backup)))
			continue
		}
		if err := item.parent.Rename(item.backup, item.leaf); err != nil {
			result = errors.Join(result, fmt.Errorf("patch rollback failed for %q; original retained at %q", item.edit.Path, path.Join(path.Dir(item.edit.Path), item.backup)))
			continue
		}
		item.backup = ""
		item.applied = false
	}
	return result
}
