package jobnotification

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Config is this plugin's own settings. Delivery goes through the mail hub: Provider names the
// provider to use, "" meaning the hub's default.
type Config struct {
	Enabled  bool     `json:"enabled"`
	TimeZone string   `json:"timeZone"`
	Provider string   `json:"provider"`
	Template Template `json:"template"`
}

// ConfigInput is the PUT body; an absent timeZone or provider keeps the stored one.
type ConfigInput struct {
	Enabled  *bool     `json:"enabled"`
	TimeZone *string   `json:"timeZone"`
	Provider *string   `json:"provider"`
	Template *Template `json:"template"`
}

func defaultConfig() Config {
	t := defaultTemplate
	t.Constants = map[string]any{}
	return Config{TimeZone: defaultTimeZone, Template: t}
}

func (in ConfigInput) validate() (Config, error) {
	var c Config
	if in.Enabled == nil || in.Template == nil {
		return c, invalid("invalid notification configuration")
	}
	c.Enabled = *in.Enabled
	var err error
	zone := defaultTimeZone
	if in.TimeZone != nil {
		zone = *in.TimeZone
	}
	if c.TimeZone, err = normalizeTimeZone(zone); err != nil {
		return c, err
	}
	if in.Provider != nil {
		if len(*in.Provider) > 64 {
			return c, invalid("invalid mail provider")
		}
		c.Provider = *in.Provider
	}
	c.Template, err = in.Template.Validate(roots)
	return c, err
}

func (s *service) loadRow(ctx context.Context) (Config, bool, error) {
	var raw string
	err := store.QueryRow(ctx, s.db, "SELECT config_json FROM v2_notification_config WHERE id = 1", nil, &raw)
	if errors.Is(err, store.ErrNoRows) {
		return defaultConfig(), false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	// Rows written before the mail hub still carry an "smtp" object; it moved to provider "default".
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, false, apperr.ErrUnavailable.Wrap(err)
	}
	if c.Template.Constants == nil {
		c.Template.Constants = map[string]any{}
	}
	return c, true, nil
}

func (s *service) save(ctx context.Context, in ConfigInput) (Config, error) {
	c, err := in.validate()
	if err != nil {
		return Config{}, err
	}
	cur, exists, err := s.loadRow(ctx)
	if err != nil {
		return Config{}, err
	}
	if exists {
		if in.TimeZone == nil {
			c.TimeZone = cur.TimeZone
		}
		if in.Provider == nil {
			c.Provider = cur.Provider
		}
	}
	if c.Enabled || c.Provider != "" {
		ok, err := s.mail.Exists(ctx, c.Provider)
		if err != nil {
			return Config{}, err
		}
		if !ok {
			if c.Provider == "" {
				return Config{}, invalid("no default mail provider is configured")
			}
			return Config{}, invalid("unknown mail provider " + c.Provider)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO v2_notification_config (id, config_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json, updated_at = excluded.updated_at`,
		string(raw), s.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return Config{}, err
	}
	c, _, err = s.loadRow(ctx)
	return c, err
}
