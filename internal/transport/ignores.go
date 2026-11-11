package transport

import (
	"context"
	"net/http"
	"net/url"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// IgnoresOrDefault возвращает настроенный в settings.json список или значения по умолчанию
// (core.DefaultIgnores). Для папок из манифеста исключения берутся из манифеста.
func (s Settings) IgnoresOrDefault() []string {
	if len(s.Ignores) > 0 {
		return s.Ignores
	}
	return core.DefaultIgnores
}

// SetIgnores записывает исключения папки через REST (заменяет весь список).
func (c *Client) SetIgnores(ctx context.Context, folderID string, lines []string) error {
	body := map[string]any{"ignore": lines}
	return c.do(ctx, http.MethodPost, "/rest/db/ignores?folder="+url.QueryEscape(folderID), body, nil)
}

// Ignores возвращает действующие исключения папки.
func (c *Client) Ignores(ctx context.Context, folderID string) ([]string, error) {
	var out struct {
		Ignore []string `json:"ignore"`
	}
	err := c.do(ctx, http.MethodGet, "/rest/db/ignores?folder="+url.QueryEscape(folderID), nil, &out)
	return out.Ignore, err
}
