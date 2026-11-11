package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client — клиент REST API Syncthing. Всегда ходит на 127.0.0.1 с API-ключом.
type Client struct {
	base string
	key  string
	http *http.Client
}

// NewClient создаёт клиента для Syncthing, слушающего GUI на 127.0.0.1:guiPort.
func NewClient(guiPort int, apiKey string) *Client {
	return &Client{
		base: "http://127.0.0.1:" + strconv.Itoa(guiPort),
		key:  apiKey,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// APIError — ответ Syncthing с кодом не 2xx.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("syncthing API: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// WaitReady ждёт, пока Syncthing начнёт отвечать на REST.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := c.do(ctx, http.MethodGet, "/rest/system/ping", nil, nil); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("Syncthing не ответил за %v: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// MyID возвращает Device ID запущенного Syncthing.
func (c *Client) MyID(ctx context.Context) (string, error) {
	var st struct {
		MyID string `json:"myID"`
	}
	err := c.do(ctx, http.MethodGet, "/rest/system/status", nil, &st)
	return st.MyID, err
}

// Connection — состояние связи с одним устройством.
type Connection struct {
	Connected     bool   `json:"connected"`
	Address       string `json:"address"`
	Type          string `json:"type"`
	ClientVersion string `json:"clientVersion"`
}

// Connections возвращает связи по Device ID.
func (c *Client) Connections(ctx context.Context) (map[string]Connection, error) {
	var out struct {
		Connections map[string]Connection `json:"connections"`
	}
	err := c.do(ctx, http.MethodGet, "/rest/system/connections", nil, &out)
	return out.Connections, err
}

// PendingDevices возвращает устройства, которые пытались подключиться, но не добавлены
// в конфиг. Syncthing только запоминает такие попытки; принимаем мы их только вручную.
func (c *Client) PendingDevices(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/rest/cluster/pending/devices", nil, &out)
	return out, err
}

// Shutdown просит Syncthing корректно завершиться.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/rest/system/shutdown", nil, nil)
}

// Конфигурации устройств и папок пока храним как map: Syncthing 2.x богат полями,
// а нам нужно менять только несколько. Остальные берём из его же шаблонов по умолчанию.
type cfgObject = map[string]any

func (c *Client) defaults(ctx context.Context, kind string) (cfgObject, error) {
	var obj cfgObject
	err := c.do(ctx, http.MethodGet, "/rest/config/defaults/"+kind, nil, &obj)
	return obj, err
}

// Device возвращает устройство из конфига или nil, если такого нет.
func (c *Client) Device(ctx context.Context, id string) (cfgObject, error) {
	var obj cfgObject
	err := c.do(ctx, http.MethodGet, "/rest/config/devices/"+url.PathEscape(id), nil, &obj)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	return obj, err
}

// PutDevice создаёт или заменяет устройство.
func (c *Client) PutDevice(ctx context.Context, id string, dev cfgObject) error {
	return c.do(ctx, http.MethodPut, "/rest/config/devices/"+url.PathEscape(id), dev, nil)
}

// Folder возвращает папку из конфига или nil.
func (c *Client) Folder(ctx context.Context, id string) (cfgObject, error) {
	var obj cfgObject
	err := c.do(ctx, http.MethodGet, "/rest/config/folders/"+url.PathEscape(id), nil, &obj)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	return obj, err
}

// PutFolder создаёт или заменяет папку.
func (c *Client) PutFolder(ctx context.Context, id string, f cfgObject) error {
	return c.do(ctx, http.MethodPut, "/rest/config/folders/"+url.PathEscape(id), f, nil)
}

// DeviceDefaults и FolderDefaults — шаблоны Syncthing для новых объектов.
func (c *Client) DeviceDefaults(ctx context.Context) (cfgObject, error) {
	return c.defaults(ctx, "device")
}
func (c *Client) FolderDefaults(ctx context.Context) (cfgObject, error) {
	return c.defaults(ctx, "folder")
}
