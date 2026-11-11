// Пакет ui — локальный веб-интерфейс агента: статус обеих машин, папки, решения предохранителя,
// конфликты, журнал, экран «что не синхронизируется».
//
// Интерфейс слушает только 127.0.0.1 и защищён от управления с посторонних сайтов:
// доступ по случайному токену (кука SameSite=Strict), проверка заголовка Host (защита от DNS rebinding)
// и обязательный служебный заголовок для изменяющих запросов.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

//go:embed static
var staticFS embed.FS

// Backend — то, что интерфейс показывает и делает. В тестах подменяется.
type Backend interface {
	Self(ctx context.Context) (Status, error)
	Others(ctx context.Context) ([]Status, error)
	Journal(n int) ([]core.Entry, error)
	Conflicts(ctx context.Context) ([]Conflict, error)
	NotSynced(ctx context.Context) ([]NotSyncedSection, error)
	SettingsPlan(ctx context.Context) ([]PlanItem, error)
	ApplySettings(ctx context.Context, module string) (string, error) // module "" = все
	KeepSettings(ctx context.Context, module string) error
	GuardAccept(ctx context.Context, folder string) error
	GuardReject(ctx context.Context, folder string) (string, error)
	SetPaused(ctx context.Context, paused bool) error
}

// Status — состояние одной машины. Свою снимает агент вживую; чужую берёт из папки состояний
// (её записывает агент той машины раз в несколько секунд), поэтому у неё есть время снимка At.
type Status struct {
	Machine   string          `json:"machine"`
	At        time.Time       `json:"at"`
	Version   string          `json:"version,omitempty"`
	Tailscale TailscaleStatus `json:"tailscale"`
	Syncthing SyncthingStatus `json:"syncthing"`
	Paused    bool            `json:"paused"`
	Guard     []GuardTrip     `json:"guard"`
	// PendingSettings — сколько модулей настроек с другой машины ждут подтверждения здесь.
	PendingSettings int `json:"pendingSettings"`
	// AutoApply — машина сама применяет свежие настройки других (без подтверждения).
	AutoApply bool `json:"autoApply"`
}

type TailscaleStatus struct {
	State string `json:"state"` // Running, Stopped, NeedsLogin...
	Name  string `json:"name"`
	IP    string `json:"ip"`
	Peers []Peer `json:"peers"`
}

type Peer struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Online bool   `json:"online"`
}

type SyncthingStatus struct {
	Running  bool           `json:"running"`
	DeviceID string         `json:"deviceId"`
	Devices  []DeviceStatus `json:"devices"`
	Folders  []FolderStatus `json:"folders"`
}

type DeviceStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Paused    bool   `json:"paused"`
	Address   string `json:"address"`
}

type FolderStatus struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	State       string `json:"state"` // idle, scanning, syncing, paused...
	Paused      bool   `json:"paused"`
	LocalFiles  int64  `json:"localFiles"`
	GlobalFiles int64  `json:"globalFiles"`
	NeedFiles   int64  `json:"needFiles"`
	NeedDeletes int64  `json:"needDeletes"`
	NeedBytes   int64  `json:"needBytes"`
	Errors      int64  `json:"errors"`
}

// GuardTrip — папка, которую предохранитель поставил на паузу и которая ждёт решения.
type GuardTrip struct {
	Folder      string    `json:"folder"`
	At          time.Time `json:"at"`
	NeedDeletes int64     `json:"needDeletes"`
	GlobalFiles int64     `json:"globalFiles"`
}

// Conflict — конфликтная копия файла (обе версии сохранены).
type Conflict struct {
	Folder   string    `json:"folder"`
	Path     string    `json:"path"`
	Original string    `json:"original"`
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size"`
}

// NotSyncedSection — раздел экрана «что не синхронизируется и почему» (данные лежат в ядре,
// их же использует команда envsync checklist).
type NotSyncedSection = core.NotSyncedSection

// PlanItem — что изменится на этой машине по настройкам другой (dry-run, только просмотр).
type PlanItem struct {
	Module  string   `json:"module"`
	Source  string   `json:"source"`
	Changes []string `json:"changes"`
	Error   string   `json:"error,omitempty"`
}

// Server — HTTP-сервер интерфейса.
type Server struct {
	B     Backend
	Token string
	port  int
}

// NewToken создаёт случайный токен доступа.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

const cookieName = "envsync_token"

// URL возвращает адрес для открытия в браузере (с токеном, он сразу уходит в куку).
func URL(port int, token string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, url.QueryEscape(token))
}

// Listen открывает порт только на loopback и возвращает слушателя.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// Serve обслуживает интерфейс на слушателе, пока не отменён ctx.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.port = l.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler возвращает обработчик со всеми проверками безопасности.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(static))

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Токен из адреса превращается в куку, адрес очищается (токен не остаётся в истории и в ссылках).
		if t := r.URL.Query().Get("t"); t != "" {
			if !s.validToken(t) {
				http.Error(w, "неверный токен", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/api/status", s.api(false, s.handleStatus))
	mux.HandleFunc("/api/journal", s.api(false, s.handleJournal))
	mux.HandleFunc("/api/conflicts", s.api(false, s.handleConflicts))
	mux.HandleFunc("/api/not-synced", s.api(false, s.handleNotSynced))
	mux.HandleFunc("/api/plan", s.api(false, s.handlePlan))
	mux.HandleFunc("/api/guard/accept", s.api(true, s.handleGuardAccept))
	mux.HandleFunc("/api/guard/reject", s.api(true, s.handleGuardReject))
	mux.HandleFunc("/api/pause", s.api(true, s.handlePause))
	mux.HandleFunc("/api/settings/apply", s.api(true, s.handleSettingsApply))
	mux.HandleFunc("/api/settings/keep", s.api(true, s.handleSettingsKeep))

	return s.hostGuard(securityHeaders(mux))
}

func (s *Server) validToken(t string) bool {
	return s.Token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

// hostGuard отклоняет запросы с чужим Host: защита от DNS rebinding (сайт злоумышленника,
// чьё имя временно указывает на 127.0.0.1).
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			host, port = r.Host, ""
		}
		ok := (host == "127.0.0.1" || host == "localhost") && (s.port == 0 || port == strconv.Itoa(s.port))
		if !ok {
			http.Error(w, "недопустимый Host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// api оборачивает обработчик проверками: токен, для изменяющих запросов ещё метод POST, служебный заголовок и Origin.
func (s *Server) api(mutating bool, h func(http.ResponseWriter, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		tok := r.Header.Get("X-EnvSync-Token")
		if err == nil && tok == "" {
			tok = c.Value
		}
		if !s.validToken(tok) {
			http.Error(w, "нет доступа: откройте интерфейс из значка в трее или командой envsync ui", http.StatusUnauthorized)
			return
		}
		if mutating {
			if r.Method != http.MethodPost {
				http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-Requested-With") != "envsync" {
				http.Error(w, "нет служебного заголовка", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "чужой Origin", http.StatusForbidden)
					return
				}
			}
		} else if r.Method != http.MethodGet {
			http.Error(w, "нужен GET", http.StatusMethodNotAllowed)
			return
		}
		out, err := h(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	self, err := s.B.Self(r.Context())
	if err != nil {
		return nil, err
	}
	others, err := s.B.Others(r.Context())
	if err != nil {
		return nil, err
	}
	if others == nil {
		others = []Status{}
	}
	return map[string]any{"self": self, "others": others}, nil
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) (any, error) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 500 {
		n = 100
	}
	entries, err := s.B.Journal(n)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []core.Entry{}
	}
	// Новые сверху.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.B.Conflicts(r.Context())
	if c == nil {
		c = []Conflict{}
	}
	return c, err
}

func (s *Server) handleNotSynced(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.B.NotSynced(r.Context())
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) (any, error) {
	p, err := s.B.SettingsPlan(r.Context())
	if p == nil {
		p = []PlanItem{}
	}
	return p, err
}

func folderParam(r *http.Request) (string, error) {
	f := strings.TrimSpace(r.URL.Query().Get("folder"))
	if f == "" || len(f) > 64 {
		return "", errors.New("не указана папка")
	}
	return f, nil
}

func (s *Server) handleGuardAccept(w http.ResponseWriter, r *http.Request) (any, error) {
	f, err := folderParam(r)
	if err != nil {
		return nil, err
	}
	if err := s.B.GuardAccept(r.Context(), f); err != nil {
		return nil, err
	}
	return map[string]string{"result": "удаление принято, папка возобновлена"}, nil
}

func (s *Server) handleGuardReject(w http.ResponseWriter, r *http.Request) (any, error) {
	f, err := folderParam(r)
	if err != nil {
		return nil, err
	}
	msg, err := s.B.GuardReject(r.Context(), f)
	if err != nil {
		return nil, err
	}
	return map[string]string{"result": msg}, nil
}

// moduleParam — имя модуля настроек или пусто (все). Разрешены только простые имена.
func moduleParam(r *http.Request) (string, error) {
	m := strings.TrimSpace(r.URL.Query().Get("module"))
	if len(m) > 32 || strings.ContainsAny(m, `/\. `) {
		return "", errors.New("недопустимое имя модуля")
	}
	return m, nil
}

func (s *Server) handleSettingsApply(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := moduleParam(r)
	if err != nil {
		return nil, err
	}
	msg, err := s.B.ApplySettings(r.Context(), m)
	if err != nil {
		return nil, err
	}
	return map[string]string{"result": msg}, nil
}

func (s *Server) handleSettingsKeep(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := moduleParam(r)
	if err != nil {
		return nil, err
	}
	if err := s.B.KeepSettings(r.Context(), m); err != nil {
		return nil, err
	}
	return map[string]string{"result": "оставлено как на этой машине; другая машина вернётся к этому варианту"}, nil
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) (any, error) {
	on := r.URL.Query().Get("on") == "1"
	if err := s.B.SetPaused(r.Context(), on); err != nil {
		return nil, err
	}
	return map[string]bool{"paused": on}, nil
}
