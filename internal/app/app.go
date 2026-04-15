package app

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/crypto/bcrypt"
)

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// CurfewWindow defines a daily period during which internet access is blocked
// regardless of remaining quota. OffTime is when the block begins; OnTime is
// when the user is allowed to turn internet back on. Both are "HH:MM" in
// 24-hour local time. An empty OffTime means no curfew for that day.
// If OnTime <= OffTime (or OnTime is empty) the window is treated as crossing
// midnight (e.g. 22:00 → 07:00 next morning).
type CurfewWindow struct {
	OffTime string `json:"off_time"` // e.g. "22:00" — empty means no curfew
	OnTime  string `json:"on_time"`  // e.g. "07:00"
}

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         Role   `json:"role"`
	// Per-day-of-week quota in minutes (index 0=Sunday … 6=Saturday).
	WeeklyQuotaMinutes [7]int `json:"weekly_quota_minutes"`
	// Per-day-of-week carryover cap in minutes.
	WeeklyCarryoverCapMinutes [7]int `json:"weekly_carryover_cap_minutes"`
	// Per-day-of-week curfew windows (0=Sunday … 6=Saturday).
	WeeklyCurfew [7]CurfewWindow `json:"weekly_curfew"`
	// Legacy scalar fields – populated when reading old state files; zeroed after migration.
	OldDailyQuotaMinutes   int   `json:"daily_quota_minutes,omitempty"`
	OldCarryoverCapMinutes int   `json:"carryover_cap_minutes,omitempty"`
	CarryoverSeconds       int64 `json:"carryover_seconds"`
	ExtraSecondsToday      int64 `json:"extra_seconds_today"`
	UsedSeconds            int64 `json:"used_seconds"`
	InternetOn             bool  `json:"internet_on"`
	LastOnUnix             int64 `json:"last_on_unix"`
}

type persistedState struct {
	CurrentDay string           `json:"current_day"`
	Users      map[string]*User `json:"users"`
}

type Session struct {
	Username  string
	ExpiresAt time.Time
}

type Config struct {
	StateFile            string
	OnScript             string
	OffScript            string
	SessionTTL           time.Duration
	InitialAdminPassword string
}

type App struct {
	mu        sync.Mutex
	state     persistedState
	sessions  map[string]Session
	quotaWake chan struct{}

	stateFile  string
	onScript   string
	offScript  string
	sessionTTL time.Duration
	logger     *log.Logger
}

type contextKey string

const userContextKey contextKey = "auth-user"

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

//go:embed web/*
var webFS embed.FS

type UserResponse struct {
	Username                  string          `json:"username"`
	Role                      Role            `json:"role"`
	WeeklyQuotaMinutes        [7]int          `json:"weekly_quota_minutes"`
	WeeklyCarryoverCapMinutes [7]int          `json:"weekly_carryover_cap_minutes"`
	WeeklyCurfew              [7]CurfewWindow `json:"weekly_curfew"`
	// Convenience fields for the current day.
	TodayQuotaMinutes        int          `json:"today_quota_minutes"`
	TodayCarryoverCapMinutes int          `json:"today_carryover_cap_minutes"`
	TodayCurfew              CurfewWindow `json:"today_curfew"`
	InCurfew                 bool         `json:"in_curfew"`
	CarryoverSeconds         int64        `json:"carryover_seconds"`
	ExtraSecondsToday        int64        `json:"extra_seconds_today"`
	TotalSeconds             int64        `json:"total_seconds"`
	UsedSeconds              int64        `json:"used_seconds"`
	RemainingSeconds         int64        `json:"remaining_seconds"`
	InternetOn               bool         `json:"internet_on"`
}

func New(cfg Config, logger *log.Logger) (*App, error) {
	if logger == nil {
		logger = log.Default()
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "./state.json"
	}
	if cfg.OnScript == "" {
		cfg.OnScript = "./on.sh"
	}
	if cfg.OffScript == "" {
		cfg.OffScript = "./off.sh"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	if cfg.InitialAdminPassword == "" {
		cfg.InitialAdminPassword = "admin"
	}

	a := &App{
		stateFile:  cfg.StateFile,
		onScript:   cfg.OnScript,
		offScript:  cfg.OffScript,
		sessionTTL: cfg.SessionTTL,
		sessions:   make(map[string]Session),
		quotaWake:  make(chan struct{}, 1),
		logger:     logger,
	}

	if err := a.loadOrInitialize(cfg.InitialAdminPassword); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	httpLog := log.New(&syslogWriter{
		tag:      "quota-http",
		priority: "daemon.info",
		fallback: a.logger,
	}, "", 0)
	r.Use(middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: httpLog, NoColor: true}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Post("/api/login", a.handleLogin)

	r.Group(func(r chi.Router) {
		r.Use(a.authMiddleware)
		r.Post("/api/logout", a.handleLogout)
		r.Get("/api/me", a.handleMe)
		r.Post("/api/me/internet/on", a.handleMyInternetOn)
		r.Post("/api/me/internet/off", a.handleMyInternetOff)

		r.Route("/api/admin", func(r chi.Router) {
			r.Use(a.adminOnly)
			r.Get("/users", a.handleAdminListUsers)
			r.Post("/users", a.handleAdminCreateUser)
			r.Put("/users/{username}/quota", a.handleAdminSetQuota)
			r.Put("/users/{username}/carryover-cap", a.handleAdminSetCarryoverCap)
			r.Put("/users/{username}/curfew", a.handleAdminSetCurfew)
			r.Post("/users/{username}/extra-minutes", a.handleAdminGrantExtraMinutes)
			r.Put("/users/{username}/password", a.handleAdminSetPassword)
		})
	})

	uiFS, err := fs.Sub(webFS, "web")
	if err != nil {
		a.logger.Printf("failed to load web ui assets: %v", err)
	} else {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, uiFS, "index.html")
		})
	}

	return r
}

func (a *App) StartBackground(ctx context.Context) {
	go func() {
		a.enforceDueQuotas()
		for {
			delay := a.nextQuotaCheckDelay()
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-a.quotaWake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				a.enforceDueQuotas()
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.cleanupSessions()
			}
		}
	}()
}

func (a *App) loadOrInitialize(initialAdminPassword string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	data, err := os.ReadFile(a.stateFile)
	if err == nil {
		var st persistedState
		if uErr := json.Unmarshal(data, &st); uErr != nil {
			return fmt.Errorf("invalid state file: %w", uErr)
		}
		if st.Users == nil {
			st.Users = make(map[string]*User)
		}
		migrateUsers(st.Users)
		a.state = st
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	hash, err := hashPassword(initialAdminPassword)
	if err != nil {
		return err
	}
	st := persistedState{
		CurrentDay: localDay(time.Now()),
		Users: map[string]*User{
			"admin": {
				Username:                  "admin",
				PasswordHash:              hash,
				Role:                      RoleAdmin,
				WeeklyQuotaMinutes:        [7]int{1440, 1440, 1440, 1440, 1440, 1440, 1440},
				WeeklyCarryoverCapMinutes: [7]int{},
			},
		},
	}
	a.state = st
	if err := a.saveLocked(); err != nil {
		return err
	}
	a.logger.Printf("initialized state with default admin user")
	return nil
}

func (a *App) saveLocked() error {
	dir := filepath.Dir(a.stateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.stateFile + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.stateFile)
}

func localDay(t time.Time) string {
	return t.Local().Format("2006-01-02")
}

// migrateUsers converts old scalar daily_quota_minutes / carryover_cap_minutes
// fields (read from pre-weekly state files) into the new per-day-of-week arrays.
// It zeroes the legacy fields after migration so they are not persisted again.
func migrateUsers(users map[string]*User) {
	for _, u := range users {
		// Migrate quota.
		if u.OldDailyQuotaMinutes > 0 {
			allZero := true
			for _, v := range u.WeeklyQuotaMinutes {
				if v != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				for i := range u.WeeklyQuotaMinutes {
					u.WeeklyQuotaMinutes[i] = u.OldDailyQuotaMinutes
				}
			}
			u.OldDailyQuotaMinutes = 0
		}
		// Migrate carryover cap.
		if u.OldCarryoverCapMinutes > 0 {
			allZero := true
			for _, v := range u.WeeklyCarryoverCapMinutes {
				if v != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				for i := range u.WeeklyCarryoverCapMinutes {
					u.WeeklyCarryoverCapMinutes[i] = u.OldCarryoverCapMinutes
				}
			}
			u.OldCarryoverCapMinutes = 0
		}
	}
}

func nextLocalMidnight(now time.Time) time.Time {
	localNow := now.Local()
	y, m, d := localNow.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, localNow.Location())
}

// weekdayIndex returns the day-of-week index (0=Sunday … 6=Saturday) for the
// local wall-clock time of t.
func weekdayIndex(t time.Time) int {
	return int(t.Local().Weekday())
}

// parseTOD parses an "HH:MM" string into hour and minute. ok is false on any
// parse error or out-of-range value.
func parseTOD(hhmm string) (hour, min int, ok bool) {
	if len(hhmm) != 5 || hhmm[2] != ':' {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hhmm[:2])
	m, err2 := strconv.Atoi(hhmm[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// todayAt returns the time.Time for HH:MM on the same local calendar day as ref.
func todayAt(ref time.Time, hour, min int) time.Time {
	loc := ref.Local().Location()
	y, mo, d := ref.Local().Date()
	return time.Date(y, mo, d, hour, min, 0, 0, loc)
}

// curfewStatus reports whether now falls inside a curfew window and returns
// the next curfew-related event time:
//   - blocked=true  → nextEvent is when the curfew window ends (CurfewOn time)
//   - blocked=false → nextEvent is when the next curfew window starts (or zero
//     if no curfew is configured in the next 7 days)
func curfewStatus(now time.Time, weekly [7]CurfewWindow) (blocked bool, nextEvent time.Time) {
	loc := now.Local().Location()

	// Check if we're still inside a curfew that started today or yesterday.
	for daysBack := 0; daysBack <= 1; daysBack++ {
		ref := now.Local().AddDate(0, 0, -daysBack)
		wd := int(ref.Weekday())
		w := weekly[wd]
		if w.OffTime == "" {
			continue
		}
		offH, offM, ok1 := parseTOD(w.OffTime)
		onH, onM, ok2 := parseTOD(w.OnTime)
		if !ok1 {
			continue
		}
		y, mo, d := ref.Date()
		offTime := time.Date(y, mo, d, offH, offM, 0, 0, loc)
		if now.Before(offTime) {
			continue // curfew hasn't started on this reference day yet
		}
		// Determine when the curfew window ends.
		var onTime time.Time
		if !ok2 {
			// No OnTime configured: treat as crossing midnight with no defined end
			// — use next midnight as the end so we don't block forever.
			onTime = time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
		} else {
			onTOD := onH*60 + onM
			offTOD := offH*60 + offM
			if onTOD <= offTOD {
				// Crosses midnight: on-time is on the next calendar day.
				onTime = time.Date(y, mo, d+1, onH, onM, 0, 0, loc)
			} else {
				onTime = time.Date(y, mo, d, onH, onM, 0, 0, loc)
			}
		}
		if now.Before(onTime) {
			return true, onTime
		}
		// This curfew window has already ended; keep checking.
	}

	// Not blocked. Find the next curfew-off event within the next 7 days.
	for daysAhead := 0; daysAhead <= 7; daysAhead++ {
		ref := now.Local().AddDate(0, 0, daysAhead)
		wd := int(ref.Weekday())
		w := weekly[wd]
		if w.OffTime == "" {
			continue
		}
		offH, offM, ok := parseTOD(w.OffTime)
		if !ok {
			continue
		}
		y, mo, d := ref.Date()
		offTime := time.Date(y, mo, d, offH, offM, 0, 0, loc)
		if offTime.After(now) {
			return false, offTime
		}
	}
	return false, time.Time{}
}

func (a *App) totalAvailableSeconds(u *User, now time.Time) int64 {
	return int64(u.WeeklyQuotaMinutes[weekdayIndex(now)])*60 + u.CarryoverSeconds + u.ExtraSecondsToday
}

func (a *App) currentUsedSeconds(u *User, nowUnix int64) int64 {
	used := u.UsedSeconds
	if u.InternetOn && u.LastOnUnix > 0 {
		delta := nowUnix - u.LastOnUnix
		if delta > 0 {
			used += delta
		}
	}
	return used
}

func (a *App) rolloverIfNeededLocked(now time.Time) bool {
	day := localDay(now)
	if a.state.CurrentDay == day {
		return false
	}

	// Determine the weekday of the day that just ended so we can use its
	// per-day quota and carryover cap when computing how much carries forward.
	prevLoc := now.Local().Location()
	prevDay, _ := time.ParseInLocation("2006-01-02", a.state.CurrentDay, prevLoc)
	prevWD := int(prevDay.Weekday())

	nowUnix := now.Unix()
	for _, u := range a.state.Users {
		used := a.currentUsedSeconds(u, nowUnix)
		// Total is computed against the day that just ended.
		total := int64(u.WeeklyQuotaMinutes[prevWD])*60 + u.CarryoverSeconds + u.ExtraSecondsToday
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		// The carryover cap is also per-day, using the day that just ended.
		carryCapSeconds := int64(u.WeeklyCarryoverCapMinutes[prevWD]) * 60
		if remaining > carryCapSeconds {
			remaining = carryCapSeconds
		}

		u.CarryoverSeconds = remaining
		u.ExtraSecondsToday = 0
		u.UsedSeconds = 0
		if u.InternetOn {
			u.LastOnUnix = nowUnix
		} else {
			u.LastOnUnix = 0
		}
	}
	a.state.CurrentDay = day
	return true
}

func (a *App) nextQuotaCheckDelay() time.Duration {
	now := time.Now()
	nowUnix := now.Unix()
	soonest := nextLocalMidnight(now)

	a.mu.Lock()
	for _, u := range a.state.Users {
		// Check curfew events for all users (online or offline).
		_, nextCurfew := curfewStatus(now, u.WeeklyCurfew)
		if !nextCurfew.IsZero() && nextCurfew.Before(soonest) {
			soonest = nextCurfew
		}

		if !u.InternetOn {
			continue
		}
		remaining := a.totalAvailableSeconds(u, now) - a.currentUsedSeconds(u, nowUnix)
		if remaining <= 0 {
			a.mu.Unlock()
			return 0
		}
		deadline := time.Unix(nowUnix+remaining, 0)
		if deadline.Before(soonest) {
			soonest = deadline
		}
	}
	a.mu.Unlock()

	delay := time.Until(soonest)
	if delay < 0 {
		return 0
	}
	return delay
}

func (a *App) signalQuotaScheduler() {
	select {
	case a.quotaWake <- struct{}{}:
	default:
	}
}

func (a *App) enforceDueQuotas() {
	now := time.Now()
	type disableEntry struct {
		username string
		reason   string
	}
	var toDisable []disableEntry
	shouldSave := false

	a.mu.Lock()
	if a.rolloverIfNeededLocked(now) {
		shouldSave = true
	}

	for _, u := range a.state.Users {
		if !u.InternetOn {
			continue
		}
		blocked, _ := curfewStatus(now, u.WeeklyCurfew)
		if blocked {
			toDisable = append(toDisable, disableEntry{u.Username, "curfew"})
			continue
		}
		if a.currentUsedSeconds(u, now.Unix()) >= a.totalAvailableSeconds(u, now) {
			toDisable = append(toDisable, disableEntry{u.Username, "quota exhausted"})
		}
	}
	if shouldSave {
		if err := a.saveLocked(); err != nil {
			a.logger.Printf("failed to persist scheduled rollover: %v", err)
		}
	}
	a.mu.Unlock()

	for _, e := range toDisable {
		if err := a.disableInternet(e.username, e.reason); err != nil {
			a.logger.Printf("auto-off failed for %s: %v", e.username, err)
		}
	}
}

func (a *App) syncUsageLocked(u *User, nowUnix int64) {
	if !u.InternetOn {
		return
	}
	if u.LastOnUnix == 0 {
		u.LastOnUnix = nowUnix
		return
	}
	delta := nowUnix - u.LastOnUnix
	if delta > 0 {
		u.UsedSeconds += delta
		u.LastOnUnix = nowUnix
	}
}

func (a *App) cleanupSessions() {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, s := range a.sessions {
		if now.After(s.ExpiresAt) {
			delete(a.sessions, token)
		}
	}
}

// syslogWriter is an io.Writer that sends each written line to the system
// syslog via the `logger` utility with the given tag and priority.
// If `logger` is unavailable it falls back to the provided logger.
type syslogWriter struct {
	tag      string
	priority string
	fallback *log.Logger
}

func (w *syslogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg == "" {
		return len(p), nil
	}
	if err := exec.Command("logger", "-t", w.tag, "-p", w.priority, msg).Run(); err != nil {
		w.fallback.Print(msg)
	}
	return len(p), nil
}

// syslog writes a message to the system syslog via the `logger` utility.
// This is available on OpenWrt (and any Linux/Unix system). On platforms
// where `logger` is not present the message falls back to the app logger.
func (a *App) syslog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := exec.Command("logger", "-t", "quota-server", "-p", "daemon.info", msg).Run(); err != nil {
		a.logger.Printf(msg)
	}
}

func (a *App) runScript(path, username string) error {
	cmd := exec.Command(path, username)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("script %s failed: %w (%s)", path, err, string(output))
	}
	return nil
}

func (a *App) enableInternet(username string) error {
	now := time.Now()
	a.mu.Lock()
	a.rolloverIfNeededLocked(now)
	u, ok := a.state.Users[username]
	if !ok {
		a.mu.Unlock()
		return errors.New("user not found")
	}
	if u.InternetOn {
		a.mu.Unlock()
		return nil
	}
	if blocked, onTime := curfewStatus(now, u.WeeklyCurfew); blocked {
		a.mu.Unlock()
		if !onTime.IsZero() {
			return fmt.Errorf("internet is blocked by curfew until %s", onTime.Local().Format("15:04"))
		}
		return errors.New("internet is blocked by curfew")
	}
	if a.currentUsedSeconds(u, now.Unix()) >= a.totalAvailableSeconds(u, now) {
		a.mu.Unlock()
		return errors.New("daily quota already exhausted")
	}
	a.mu.Unlock()

	if err := a.runScript(a.onScript, username); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok = a.state.Users[username]
	if !ok {
		return errors.New("user not found")
	}
	u.InternetOn = true
	u.LastOnUnix = time.Now().Unix()
	if err := a.saveLocked(); err != nil {
		return err
	}
	a.syslog("internet enabled for %s", username)
	a.signalQuotaScheduler()
	return nil
}

func (a *App) disableInternet(username, reason string) error {
	a.mu.Lock()
	u, ok := a.state.Users[username]
	if !ok {
		a.mu.Unlock()
		return errors.New("user not found")
	}
	if !u.InternetOn {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	if err := a.runScript(a.offScript, username); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok = a.state.Users[username]
	if !ok {
		return errors.New("user not found")
	}
	a.syncUsageLocked(u, time.Now().Unix())
	u.InternetOn = false
	u.LastOnUnix = 0
	if err := a.saveLocked(); err != nil {
		return err
	}
	a.syslog("internet disabled for %s (%s)", username, reason)
	a.signalQuotaScheduler()
	return nil
}

func hashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func verifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (a *App) authenticate(username, password string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.state.Users[username]
	if !ok {
		return false
	}
	return verifyPassword(u.PasswordHash, password)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *App) createSession(username string) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = Session{
		Username:  username,
		ExpiresAt: time.Now().Add(a.sessionTTL),
	}
	return token, nil
}

func (a *App) userFromToken(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(s.ExpiresAt) {
		delete(a.sessions, token)
		return "", false
	}
	return s.Username, true
}

func (a *App) deleteSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_token")
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		username, ok := a.userFromToken(cookie.Value)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid session")
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := authenticatedUsername(r.Context())
		a.mu.Lock()
		u, ok := a.state.Users[username]
		a.mu.Unlock()
		if !ok || u.Role != RoleAdmin {
			writeError(w, http.StatusForbidden, "admin required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authenticatedUsername(ctx context.Context) string {
	username, _ := ctx.Value(userContextKey).(string)
	return username
}

func (a *App) userResponse(username string) (UserResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.rolloverIfNeededLocked(now)
	u, ok := a.state.Users[username]
	if !ok {
		return UserResponse{}, errors.New("user not found")
	}
	used := a.currentUsedSeconds(u, now.Unix())
	total := a.totalAvailableSeconds(u, now)
	remaining := total - used
	if remaining < 0 {
		remaining = 0
	}
	wd := weekdayIndex(now)
	inCurfew, _ := curfewStatus(now, u.WeeklyCurfew)
	return UserResponse{
		Username:                  u.Username,
		Role:                      u.Role,
		WeeklyQuotaMinutes:        u.WeeklyQuotaMinutes,
		WeeklyCarryoverCapMinutes: u.WeeklyCarryoverCapMinutes,
		WeeklyCurfew:              u.WeeklyCurfew,
		TodayQuotaMinutes:         u.WeeklyQuotaMinutes[wd],
		TodayCarryoverCapMinutes:  u.WeeklyCarryoverCapMinutes[wd],
		TodayCurfew:               u.WeeklyCurfew[wd],
		InCurfew:                  inCurfew,
		CarryoverSeconds:          u.CarryoverSeconds,
		ExtraSecondsToday:         u.ExtraSecondsToday,
		TotalSeconds:              total,
		UsedSeconds:               used,
		RemainingSeconds:          remaining,
		InternetOn:                u.InternetOn,
	}, nil
}

func (a *App) listUsers() []UserResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.rolloverIfNeededLocked(now)
	responses := make([]UserResponse, 0, len(a.state.Users))
	nowUnix := now.Unix()
	wd := weekdayIndex(now)
	for _, u := range a.state.Users {
		used := a.currentUsedSeconds(u, nowUnix)
		total := a.totalAvailableSeconds(u, now)
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		inCurfew, _ := curfewStatus(now, u.WeeklyCurfew)
		responses = append(responses, UserResponse{
			Username:                  u.Username,
			Role:                      u.Role,
			WeeklyQuotaMinutes:        u.WeeklyQuotaMinutes,
			WeeklyCarryoverCapMinutes: u.WeeklyCarryoverCapMinutes,
			WeeklyCurfew:              u.WeeklyCurfew,
			TodayQuotaMinutes:         u.WeeklyQuotaMinutes[wd],
			TodayCarryoverCapMinutes:  u.WeeklyCarryoverCapMinutes[wd],
			TodayCurfew:               u.WeeklyCurfew[wd],
			InCurfew:                  inCurfew,
			CarryoverSeconds:          u.CarryoverSeconds,
			ExtraSecondsToday:         u.ExtraSecondsToday,
			TotalSeconds:              total,
			UsedSeconds:               used,
			RemainingSeconds:          remaining,
			InternetOn:                u.InternetOn,
		})
	}
	return responses
}

func (a *App) createUser(username, password string, role Role, weeklyQuotaMinutes, weeklyCarryoverCapMinutes [7]int, weeklyCurfew [7]CurfewWindow) error {
	if username == "" || password == "" {
		return errors.New("username and password required")
	}
	for _, m := range weeklyQuotaMinutes {
		if m < 0 {
			return errors.New("quota must be >= 0")
		}
	}
	for _, m := range weeklyCarryoverCapMinutes {
		if m < 0 {
			return errors.New("carryover cap must be >= 0")
		}
	}
	for i, w := range weeklyCurfew {
		if w.OffTime == "" {
			continue
		}
		if _, _, ok := parseTOD(w.OffTime); !ok {
			return fmt.Errorf("day %d: invalid curfew off_time %q", i, w.OffTime)
		}
		if w.OnTime != "" {
			if _, _, ok := parseTOD(w.OnTime); !ok {
				return fmt.Errorf("day %d: invalid curfew on_time %q", i, w.OnTime)
			}
		}
	}
	if role != RoleAdmin && role != RoleUser {
		return errors.New("invalid role")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.state.Users[username]; exists {
		return errors.New("user already exists")
	}
	a.state.Users[username] = &User{
		Username:                  username,
		PasswordHash:              hash,
		Role:                      role,
		WeeklyQuotaMinutes:        weeklyQuotaMinutes,
		WeeklyCarryoverCapMinutes: weeklyCarryoverCapMinutes,
		WeeklyCurfew:              weeklyCurfew,
	}
	if err := a.saveLocked(); err != nil {
		return err
	}
	a.signalQuotaScheduler()
	return nil
}

func (a *App) setPassword(username, password string) error {
	if password == "" {
		return errors.New("password required")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.state.Users[username]
	if !ok {
		return errors.New("user not found")
	}
	u.PasswordHash = hash
	return a.saveLocked()
}

func (a *App) setWeeklyQuota(username string, weeklyMinutes [7]int) error {
	for _, m := range weeklyMinutes {
		if m < 0 {
			return errors.New("quota must be >= 0")
		}
	}
	shouldDisable := false
	a.mu.Lock()
	now := time.Now()
	a.rolloverIfNeededLocked(now)
	u, ok := a.state.Users[username]
	if !ok {
		a.mu.Unlock()
		return errors.New("user not found")
	}
	u.WeeklyQuotaMinutes = weeklyMinutes
	if u.InternetOn && a.currentUsedSeconds(u, now.Unix()) >= a.totalAvailableSeconds(u, now) {
		shouldDisable = true
	}
	if err := a.saveLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	if shouldDisable {
		if err := a.disableInternet(username, "quota reduced"); err != nil {
			return err
		}
	}
	a.signalQuotaScheduler()
	return nil
}

func (a *App) setWeeklyCarryoverCap(username string, weeklyCapMinutes [7]int) error {
	for _, m := range weeklyCapMinutes {
		if m < 0 {
			return errors.New("carryover cap must be >= 0")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.rolloverIfNeededLocked(now)
	u, ok := a.state.Users[username]
	if !ok {
		return errors.New("user not found")
	}
	u.WeeklyCarryoverCapMinutes = weeklyCapMinutes
	// Clamp existing carryover to today's new cap to avoid immediate excess.
	maxCarryover := int64(weeklyCapMinutes[weekdayIndex(now)]) * 60
	if u.CarryoverSeconds > maxCarryover {
		u.CarryoverSeconds = maxCarryover
	}
	if err := a.saveLocked(); err != nil {
		return err
	}
	a.signalQuotaScheduler()
	return nil
}

func (a *App) setCurfew(username string, weekly [7]CurfewWindow) error {
	// Validate all provided windows.
	for i, w := range weekly {
		if w.OffTime == "" {
			continue // no curfew for this day — OK
		}
		if _, _, ok := parseTOD(w.OffTime); !ok {
			return fmt.Errorf("day %d: invalid off_time %q (expected HH:MM)", i, w.OffTime)
		}
		if w.OnTime != "" {
			if _, _, ok := parseTOD(w.OnTime); !ok {
				return fmt.Errorf("day %d: invalid on_time %q (expected HH:MM)", i, w.OnTime)
			}
		}
	}

	shouldDisable := false
	now := time.Now()
	a.mu.Lock()
	u, ok := a.state.Users[username]
	if !ok {
		a.mu.Unlock()
		return errors.New("user not found")
	}
	u.WeeklyCurfew = weekly
	if u.InternetOn {
		if blocked, _ := curfewStatus(now, u.WeeklyCurfew); blocked {
			shouldDisable = true
		}
	}
	if err := a.saveLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	if shouldDisable {
		if err := a.disableInternet(username, "curfew"); err != nil {
			return err
		}
	}
	a.signalQuotaScheduler()
	return nil
}

func (a *App) grantExtraMinutes(username string, minutes int) error {
	shouldDisable := false
	now := time.Now()
	a.mu.Lock()
	a.rolloverIfNeededLocked(now)
	u, ok := a.state.Users[username]
	if !ok {
		a.mu.Unlock()
		return errors.New("user not found")
	}
	u.ExtraSecondsToday += int64(minutes) * 60
	if u.InternetOn && a.currentUsedSeconds(u, now.Unix()) >= a.totalAvailableSeconds(u, now) {
		shouldDisable = true
	}
	if err := a.saveLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	if shouldDisable {
		if err := a.disableInternet(username, "day adjustment reduced available time"); err != nil {
			return err
		}
	}
	a.signalQuotaScheduler()
	return nil
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !a.authenticate(req.Username, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := a.createSession(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(a.sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "logged in"})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil && cookie.Value != "" {
		a.deleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "logged out"})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	username := authenticatedUsername(r.Context())
	resp, err := a.userResponse(username)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleMyInternetOn(w http.ResponseWriter, r *http.Request) {
	username := authenticatedUsername(r.Context())
	if err := a.enableInternet(username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "internet enabled"})
}

func (a *App) handleMyInternetOff(w http.ResponseWriter, r *http.Request) {
	username := authenticatedUsername(r.Context())
	if err := a.disableInternet(username, "user request"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "internet disabled"})
}

type createUserRequest struct {
	Username                  string          `json:"username"`
	Password                  string          `json:"password"`
	Role                      Role            `json:"role"`
	WeeklyQuotaMinutes        [7]int          `json:"weekly_quota_minutes"`
	WeeklyCarryoverCapMinutes [7]int          `json:"weekly_carryover_cap_minutes"`
	WeeklyCurfew              [7]CurfewWindow `json:"weekly_curfew"`
}

func (a *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Role == "" {
		req.Role = RoleUser
	}
	if err := a.createUser(req.Username, req.Password, req.Role, req.WeeklyQuotaMinutes, req.WeeklyCarryoverCapMinutes, req.WeeklyCurfew); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "user created"})
}

func (a *App) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.listUsers())
}

type setWeeklyQuotaRequest struct {
	WeeklyQuotaMinutes [7]int `json:"weekly_quota_minutes"`
}

type setWeeklyCarryoverCapRequest struct {
	WeeklyCarryoverCapMinutes [7]int `json:"weekly_carryover_cap_minutes"`
}

type grantExtraMinutesRequest struct {
	Minutes int `json:"minutes"`
}

type setCurfewRequest struct {
	WeeklyCurfew [7]CurfewWindow `json:"weekly_curfew"`
}

func (a *App) handleAdminSetCurfew(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req setCurfewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.setCurfew(username, req.WeeklyCurfew); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "curfew updated"})
}

func (a *App) handleAdminSetQuota(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req setWeeklyQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.setWeeklyQuota(username, req.WeeklyQuotaMinutes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "quota updated"})
}

func (a *App) handleAdminSetCarryoverCap(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req setWeeklyCarryoverCapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.setWeeklyCarryoverCap(username, req.WeeklyCarryoverCapMinutes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "carryover cap updated"})
}

func (a *App) handleAdminGrantExtraMinutes(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req grantExtraMinutesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.grantExtraMinutes(username, req.Minutes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "today minutes adjusted"})
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

func (a *App) handleAdminSetPassword(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req setPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.setPassword(username, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "password updated"})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
