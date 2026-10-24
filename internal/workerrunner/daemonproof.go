package workerrunner

// This helper deliberately does not use net/http.Transport: a transport may
// reconnect after EOF. dial-stdio opens one daemon connection and copies bytes;
// requests below use that single pipe until it is closed, with no retry.
import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"regexp"
	"time"
)

type daemonSession interface {
	identity() (string, error)
	container(string) (daemonContainer, bool, error)
	close()
}
type daemonContainer struct {
	ID     string `json:"Id"`
	Name   string
	Config struct{ Labels map[string]string }
}
type daemonPipe struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	reader *bufio.Reader
	cancel context.CancelFunc
}

var resourceNamePattern = regexp.MustCompile(`^rcp-[a-f0-9]{32}-(check|buildkit)$`)
var daemonIDPattern = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,128}$`)

func openDaemon(d docker) (daemonSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cmd := d.command(ctx, "system", "dial-stdio")
	in, e := cmd.StdinPipe()
	if e != nil {
		cancel()
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		in.Close()
		cancel()
		return nil, e
	}
	cmd.Stderr = &boundedBuffer{remaining: 4096}
	if e = cmd.Start(); e != nil {
		in.Close()
		cancel()
		return nil, e
	}
	return &daemonPipe{cmd: cmd, input: in, reader: bufio.NewReader(io.LimitReader(out, 4<<20)), cancel: cancel}, nil
}
func (p *daemonPipe) close() { p.input.Close(); p.cancel(); _ = p.cmd.Wait() }
func (p *daemonPipe) get(path string, value any) (bool, error) {
	req, _ := http.NewRequest("GET", "http://docker"+path, nil)
	if e := req.Write(p.input); e != nil {
		return false, errors.New("daemon connection failed")
	}
	res, e := http.ReadResponse(p.reader, req)
	if e != nil {
		return false, errors.New("daemon response failed")
	}
	defer res.Body.Close()
	// Close means there cannot be another verified observation on this session.
	if res.Close {
		return false, errors.New("daemon connection is not persistent")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return false, errors.New("daemon response exceeds bound")
	}
	if res.StatusCode == 404 {
		return false, nil
	}
	if res.StatusCode != 200 || !uniqueJSONFields(b) || !canonicalStructFields(b, reflect.TypeOf(value)) || json.Unmarshal(b, value) != nil {
		return false, errors.New("daemon observation failed")
	}
	return true, nil
}
func (p *daemonPipe) identity() (string, error) {
	var v struct{ ID string }
	ok, e := p.get("/info", &v)
	if e != nil || !ok || !daemonIDPattern.MatchString(v.ID) {
		return "", errors.New("daemon identity unavailable")
	}
	return v.ID, nil
}
func (p *daemonPipe) container(id string) (daemonContainer, bool, error) {
	var v daemonContainer
	if !containerIDPattern.MatchString(id) && !resourceNamePattern.MatchString(id) {
		return v, false, errors.New("full container identity required")
	}
	ok, e := p.get("/containers/"+id+"/json", &v)
	return v, ok, e
}
func presence(d docker, r ownedResource, owner string) (*presenceProof, error) {
	s, e := openDaemon(d)
	if e != nil {
		return nil, e
	}
	defer s.close()
	return presenceOn(s, r, owner)
}
func presenceOn(s daemonSession, r ownedResource, owner string) (*presenceProof, error) {
	daemon, e := s.identity()
	if e != nil {
		return nil, e
	}
	v, ok, e := s.container(r.ID)
	if e != nil || !ok || v.ID != r.ID || v.Name != "/"+r.Name || v.Config.Labels["rcp.worker.owner"] != owner {
		return nil, errors.New("daemon-bound ownership not proven")
	}
	return &presenceProof{DaemonID: daemon, ID: r.ID, Name: r.Name, Owner: owner, ObservedAt: time.Now().UTC()}, nil
}
