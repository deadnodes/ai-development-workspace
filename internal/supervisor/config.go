package supervisor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type Config struct {
	RCPURL       string        `json:"rcp_url"`
	Profile      string        `json:"profile,omitempty"`
	TokenEnv     string        `json:"token_env,omitempty"`
	SupervisorID string        `json:"supervisor_id"`
	ProductIDs   []string      `json:"product_ids"`
	PollInterval string        `json:"poll_interval,omitempty"`
	IdleWindow   string        `json:"idle_window,omitempty"`
	WakeTimeout  string        `json:"wake_timeout,omitempty"`
	RateLimit    string        `json:"rate_limit,omitempty"`
	AllowedKinds []string      `json:"allowed_kinds,omitempty"`
	StateFile    string        `json:"state_file,omitempty"`
	TmuxPath     string        `json:"tmux_path,omitempty"`
	CodexPath    string        `json:"codex_path,omitempty"`
	ClaudePath   string        `json:"claude_path,omitempty"`
	PgrepPath    string        `json:"pgrep_path,omitempty"`
	PollEvery    time.Duration `json:"-"`
	IdleFor      time.Duration `json:"-"`
	WakeFor      time.Duration `json:"-"`
	RateFor      time.Duration `json:"-"`
}

var (
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	sessionPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,128}$`)
)

func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "release-control", "supervisor.json"), nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid supervisor config")
	}
	if err = c.Normalize(); err != nil {
		return c, err
	}
	if c.StateFile == "" {
		c.StateFile = path + ".state.json"
	}
	return c, nil
}

func (c *Config) Normalize() error {
	if c.RCPURL == "" || c.SupervisorID == "" || len(c.ProductIDs) == 0 || (c.Profile != "" && c.TokenEnv != "") {
		return errors.New("rcp_url, supervisor_id and product_ids are required; choose profile or token_env")
	}
	if c.PollInterval == "" {
		c.PollInterval = "15s"
	}
	if c.IdleWindow == "" {
		c.IdleWindow = "5s"
	}
	if c.WakeTimeout == "" {
		c.WakeTimeout = "120s"
	}
	poll, err := time.ParseDuration(c.PollInterval)
	if err != nil {
		return errors.New("invalid poll_interval")
	}
	idle, err := time.ParseDuration(c.IdleWindow)
	if err != nil {
		return errors.New("invalid idle_window")
	}
	wake, err := time.ParseDuration(c.WakeTimeout)
	if err != nil {
		return errors.New("invalid wake_timeout")
	}
	rate := idle * 6
	if c.RateLimit != "" {
		rate, err = time.ParseDuration(c.RateLimit)
		if err != nil {
			return errors.New("invalid rate_limit")
		}
	}
	if poll <= 0 || poll > time.Hour || idle <= 0 || idle > 10*time.Minute || wake <= 0 || wake > time.Hour || rate < 0 || rate > 24*time.Hour {
		return errors.New("invalid supervisor timing configuration")
	}
	c.PollEvery, c.IdleFor, c.WakeFor, c.RateFor = poll, idle, wake, rate
	if c.SupervisorID == "" || len(c.SupervisorID) > 128 {
		return errors.New("invalid supervisor_id")
	}
	for _, id := range c.ProductIDs {
		if !idPattern.MatchString(id) {
			return errors.New("invalid product id")
		}
	}
	if len(c.AllowedKinds) == 0 {
		c.AllowedKinds = []string{"codex", "claude_code", "claude_desktop", "ccr", "other"}
	}
	for _, k := range c.AllowedKinds {
		if k != "codex" && k != "claude_code" && k != "claude_desktop" && k != "ccr" && k != "other" {
			return errors.New("invalid allowed harness kind")
		}
	}
	return nil
}

func ValidSessionRef(s string) bool { return sessionPattern.MatchString(s) }
