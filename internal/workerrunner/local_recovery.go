package workerrunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type presenceProof struct {
	DaemonID   string    `json:"daemon_id"`
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	ObservedAt time.Time `json:"observed_at"`
}
type closureResource struct {
	ID       string `json:"id"`
	State    string `json:"create_state"`
	DaemonID string `json:"daemon_id"`
}
type cleanupClosure struct {
	Owner            string            `json:"owner"`
	PreconditionHash string            `json:"precondition_hash"`
	OperatorRef      string            `json:"operator_ref"`
	VerifiedAt       time.Time         `json:"verified_at"`
	Result           string            `json:"result"`
	Resources        []closureResource `json:"resources"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var operatorPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_/-]{0,63}$`)

func validPresence(p *presenceProof, r ownedResource, owner string) bool {
	return containerIDPattern.MatchString(r.ID) && p.ID == r.ID && p.Name == r.Name && p.Owner == owner && daemonIDPattern.MatchString(p.DaemonID) && !p.ObservedAt.IsZero()
}
func validClosure(j journal) bool {
	c := j.Closure
	if c == nil {
		return true
	}
	if !j.Done || c.Owner != j.Owner || !hashPattern.MatchString(c.PreconditionHash) || !operatorPattern.MatchString(c.OperatorRef) || c.VerifiedAt.IsZero() || c.Result != "EXACT_RESOURCES_ABSENT" || len(c.Resources) != len(j.Resources) || len(c.Resources) == 0 {
		return false
	}
	original := j
	original.Closure = nil
	original.Done = false
	raw, e := json.Marshal(original)
	if e != nil {
		return false
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != c.PreconditionHash {
		return false
	}
	var daemon string
	for i, r := range j.Resources {
		p := r.Presence
		v := c.Resources[i]
		if p != nil {
			if daemon != "" && daemon != p.DaemonID {
				return false
			}
			daemon = p.DaemonID
		}
		if p == nil || !validPresence(p, r, j.Owner) || v.ID != r.ID || v.State != r.CreateState || v.DaemonID != p.DaemonID || c.VerifiedAt.Before(p.ObservedAt) || c.VerifiedAt.Before(j.LeaseUntil) || c.VerifiedAt.Before(j.Deadline) {
			return false
		}
	}
	return true
}

// LocalCleanupInspection is deliberately independent of server job state. It
// excludes Docker paths, mutable names, source, reports and provider payloads.
type LocalCleanupInspection struct {
	Owner            string                    `json:"owner"`
	JournalHash      string                    `json:"journal_hash"`
	Done             bool                      `json:"done"`
	Resources        []LocalResourceInspection `json:"resources"`
	Eligible         bool                      `json:"closure_eligible"`
	Reasons          []string                  `json:"reasons"`
	ServerReconciled bool                      `json:"server_reconciled"`
}
type LocalResourceInspection struct {
	ID       string `json:"id,omitempty"`
	State    string `json:"create_state"`
	DaemonID string `json:"daemon_id,omitempty"`
}

type localBinding struct {
	state, attempt         *os.Root
	stateInfo, attemptInfo os.FileInfo
	statePath, attemptPath string
	stateLock, journalLock *os.File
}

func privateInfo(info os.FileInfo, dir bool) bool {
	if info == nil || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (dir || stat.Nlink == 1)
}
func lockAt(root *os.Root, name string) (*os.File, error) {
	f, e := root.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, errors.New("local lock unavailable")
	}
	info, e := f.Stat()
	if e != nil || !privateInfo(info, false) {
		f.Close()
		return nil, errors.New("unsafe local lock")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("local state busy")
	}
	return f, nil
}
func (b *localBinding) close() {
	if b.journalLock != nil {
		b.journalLock.Close()
	}
	if b.stateLock != nil {
		b.stateLock.Close()
	}
	if b.attempt != nil {
		b.attempt.Close()
	}
	if b.state != nil {
		b.state.Close()
	}
}
func bindLocal(state, owner string) (b *localBinding, err error) {
	if !ownerPattern.MatchString(owner) || !filepath.IsAbs(state) || filepath.Clean(state) != state {
		return nil, errors.New("canonical state directory and exact owner required")
	}
	canonical, e := filepath.EvalSymlinks(state)
	if e != nil || canonical != state {
		return nil, errors.New("state path is not canonical")
	}
	b = &localBinding{statePath: state, attemptPath: filepath.Join(state, owner)}
	defer func() {
		if err != nil {
			b.close()
		}
	}()
	b.stateInfo, e = os.Lstat(state)
	if e != nil || !privateInfo(b.stateInfo, true) {
		return b, errors.New("unsafe state directory")
	}
	b.state, e = os.OpenRoot(state)
	if e != nil {
		return b, errors.New("state unavailable")
	}
	b.stateLock, e = lockAt(b.state, ".lock")
	if e != nil {
		return b, e
	}
	b.attemptInfo, e = b.state.Lstat(owner)
	if e != nil || !privateInfo(b.attemptInfo, true) {
		return b, errors.New("unsafe owner directory")
	}
	b.attempt, e = b.state.OpenRoot(owner)
	if e != nil {
		return b, errors.New("owner directory unavailable")
	}
	b.journalLock, e = lockAt(b.attempt, "journal.json.lock")
	if e != nil {
		return b, e
	}
	if e = b.check(); e != nil {
		return b, e
	}
	return b, nil
}
func (b *localBinding) check() error {
	for _, v := range []struct {
		root *os.Root
		info os.FileInfo
	}{{b.state, b.stateInfo}, {b.attempt, b.attemptInfo}} {
		f, e := v.root.Open(".")
		if e != nil {
			return errors.New("directory binding unavailable")
		}
		info, e := f.Stat()
		f.Close()
		if e != nil || !os.SameFile(info, v.info) {
			return errors.New("directory binding changed")
		}
	}
	a, e := os.Lstat(b.statePath)
	if e != nil || !os.SameFile(a, b.stateInfo) || !privateInfo(a, true) {
		return errors.New("state path changed")
	}
	canonical, e := filepath.EvalSymlinks(b.statePath)
	if e != nil || canonical != b.statePath {
		return errors.New("state path changed")
	}
	a, e = b.state.Lstat(filepath.Base(b.attemptPath))
	if e != nil || !os.SameFile(a, b.attemptInfo) || !privateInfo(a, true) {
		return errors.New("owner path changed")
	}
	for _, v := range []struct {
		root *os.Root
		name string
		file *os.File
	}{{b.state, ".lock", b.stateLock}, {b.attempt, "journal.json.lock", b.journalLock}} {
		a, e = v.root.Lstat(v.name)
		f, fe := v.file.Stat()
		if e != nil || fe != nil || !os.SameFile(a, f) || !privateInfo(a, false) {
			return errors.New("lock path changed")
		}
	}
	return nil
}
func (b *localBinding) read() (journal, string, os.FileInfo, error) {
	var j journal
	f, e := b.attempt.OpenFile("journal.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return j, "", nil, errors.New("journal unavailable")
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !privateInfo(info, false) || info.Size() > 8192 {
		return j, "", nil, errors.New("unsafe journal")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 8193))
	if e != nil {
		return j, "", nil, errors.New("journal read failed")
	}
	j, e = decodeJournal(raw)
	if e != nil {
		return j, "", nil, errors.New("invalid local journal")
	}
	current, e := b.attempt.Lstat("journal.json")
	if e != nil || !os.SameFile(info, current) || j.Owner != filepath.Base(b.attemptPath) {
		return j, "", nil, errors.New("journal path changed")
	}
	sum := sha256.Sum256(raw)
	return j, hex.EncodeToString(sum[:]), info, nil
}
func inspectLocal(j journal, hash string) LocalCleanupInspection {
	v := LocalCleanupInspection{Owner: j.Owner, JournalHash: hash, Done: j.Done, Resources: []LocalResourceInspection{}, Reasons: []string{}}
	if j.Closure != nil {
		v.Reasons = append(v.Reasons, "ALREADY_CLOSED")
	}
	if j.Done {
		v.Reasons = append(v.Reasons, "ALREADY_DONE")
	}
	if !parentDead(j.ParentPID) {
		v.Reasons = append(v.Reasons, "SUPERVISOR_LIVE_OR_UNKNOWN")
	}
	now := time.Now()
	if now.Before(j.LeaseUntil) {
		v.Reasons = append(v.Reasons, "LEASE_NOT_EXPIRED")
	}
	if now.Before(j.Deadline) {
		v.Reasons = append(v.Reasons, "DEADLINE_NOT_EXPIRED")
	}
	if len(j.Resources) == 0 {
		v.Reasons = append(v.Reasons, "NO_RESOURCE_PROOF")
	}
	for _, r := range j.Resources {
		x := LocalResourceInspection{ID: r.ID, State: r.CreateState}
		if r.Presence != nil {
			x.DaemonID = r.Presence.DaemonID
		} else {
			v.Reasons = append(v.Reasons, "PRESENCE_UNKNOWN")
		}
		if r.ID == "" {
			v.Reasons = append(v.Reasons, "RESOURCE_ID_UNKNOWN")
		}
		v.Resources = append(v.Resources, x)
	}
	v.Eligible = len(v.Reasons) == 0
	return v
}
func InspectLocalCleanup(state, owner string) (LocalCleanupInspection, error) {
	b, e := bindLocal(state, owner)
	if e != nil {
		return LocalCleanupInspection{}, e
	}
	defer b.close()
	j, h, _, e := b.read()
	if e != nil {
		return LocalCleanupInspection{}, e
	}
	if e = b.check(); e != nil {
		return LocalCleanupInspection{}, e
	}
	return inspectLocal(j, h), nil
}
func verifyAbsent(s daemonSession, j journal) error {
	daemon, e := s.identity()
	if e != nil {
		return errors.New("daemon identity unknown")
	}
	for _, r := range j.Resources {
		if r.Presence == nil || !validPresence(r.Presence, r, j.Owner) || r.Presence.DaemonID != daemon {
			return errors.New("daemon presence proof mismatch")
		}
		_, present, e := s.container(r.ID)
		if e != nil || present {
			return errors.New("exact resource absence not proven")
		}
		_, sameName, e := s.container(r.Name)
		if e != nil || sameName {
			return errors.New("resource name absence not proven")
		}
	}
	return nil
}
func ReconcileLocalCleanup(state, owner, expectedHash, operator string) (LocalCleanupInspection, error) {
	return reconcileLocalCleanup(state, owner, expectedHash, operator, openDaemon)
}
func reconcileLocalCleanup(state, owner, expectedHash, operator string, open func(docker) (daemonSession, error)) (LocalCleanupInspection, error) {
	var empty LocalCleanupInspection
	if !hashPattern.MatchString(expectedHash) || !operatorPattern.MatchString(operator) {
		return empty, errors.New("exact hash and non-sensitive operator reference required")
	}
	b, e := bindLocal(state, owner)
	if e != nil {
		return empty, e
	}
	defer b.close()
	j, h, info, e := b.read()
	if e != nil {
		return empty, e
	}
	if j.Closure != nil {
		if j.Closure.PreconditionHash == expectedHash && j.Closure.OperatorRef == operator {
			return inspectLocal(j, h), nil
		}
		return empty, errors.New("conflicting cleanup closure")
	}
	canonical, _ := json.Marshal(j)
	canonicalHash := sha256.Sum256(canonical)
	if hex.EncodeToString(canonicalHash[:]) != h {
		return empty, errors.New("journal encoding is not canonical")
	}
	if h != expectedHash {
		return empty, errors.New("journal hash changed")
	}
	v := inspectLocal(j, h)
	if !v.Eligible {
		return v, errors.New("local cleanup closure preconditions unknown or unmet")
	}
	s, e := open(docker{context: j.DockerContext, config: j.DockerConfig})
	if e != nil {
		return v, errors.New("daemon verification unavailable")
	}
	defer s.close()
	if e = verifyAbsent(s, j); e != nil {
		return v, e
	}
	if e = b.check(); e != nil {
		return v, e
	}
	_, again, latest, e := b.read()
	if e != nil || again != h || !os.SameFile(info, latest) {
		return v, errors.New("journal changed during verification")
	}
	c := &cleanupClosure{Owner: owner, PreconditionHash: h, OperatorRef: operator, VerifiedAt: time.Now().UTC(), Result: "EXACT_RESOURCES_ABSENT"}
	for _, r := range j.Resources {
		c.Resources = append(c.Resources, closureResource{ID: r.ID, State: r.CreateState, DaemonID: r.Presence.DaemonID})
	}
	j.Closure = c
	j.Done = true
	if !validClosure(j) {
		return v, errors.New("cleanup closure validation failed")
	}
	raw, e := json.Marshal(j)
	if e != nil || len(raw) > 8192 {
		return v, errors.New("closure exceeds journal bound")
	}
	temp := ".closure-" + nonce()
	f, e := b.attempt.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return v, errors.New("closure staging failed")
	}
	defer b.attempt.Remove(temp)
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return v, errors.New("closure durability failed")
	}
	if e = b.check(); e != nil {
		return v, e
	}
	if e = b.attempt.Rename(temp, "journal.json"); e != nil {
		return v, errors.New("closure commit failed")
	}
	dir, e := b.attempt.Open(".")
	if e != nil {
		return v, errors.New("closure sync failed")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return v, errors.New("closure sync failed")
	}
	_, after, _, e := b.read()
	if e != nil {
		return v, e
	}
	return inspectLocal(j, after), nil
}

// encoding/json accepts duplicate object keys by default; durable evidence must
// have one interpretation, including nested proof/report objects.
func uniqueJSONFields(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, e := d.Token()
		if e != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				name, ok := key.(string)
				if e != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		}
		return false
	}
	return value(0) && func() bool { _, e := d.Token(); return e == io.EOF }()
}

// Struct fields require their exact wire spelling. encoding/json otherwise
// matches case aliases (including aliases of explicitly tagged fields). Maps
// retain exact, case-sensitive keys: Docker labels may legitimately contain X
// and x, and must not be mistaken for struct field aliases.
func canonicalStructFields(raw []byte, target reflect.Type) bool {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(target).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	switch target.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return false
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < target.NumField(); i++ {
			f := target.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		for key, value := range object {
			field, ok := fields[key]
			if !ok {
				for name := range fields {
					if strings.EqualFold(key, name) {
						return false
					}
				}
				continue
			}
			if !canonicalStructFields(value, field) {
				return false
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return false
		}
		for _, value := range object {
			if !canonicalStructFields(value, target.Elem()) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		// []byte is represented as a base64 JSON string by encoding/json.
		if target.Elem().Kind() == reflect.Uint8 {
			return true
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if !canonicalStructFields(value, target.Elem()) {
				return false
			}
		}
	}
	return true
}
