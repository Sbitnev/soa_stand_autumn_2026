package main

// Режим сервиса (runner -serve :8081, сервис runner-ui, make ui): прогоны
// сценариев по кнопке с дашборда. Сам прогон — тот же r.run, что и в
// make scenarios; сервис только запускает его в горутине и отдаёт ход и итог.
//
//	POST /run {"only": "S1,S5", "wait": 30} — запустить прогон (409, если идёт)
//	GET  /status                            — ход и итог последнего прогона
//	GET  /report                            — HTML-отчёт последнего прогона
//	GET  /params                            — открытые параметры сценариев
//	                                          (для кнопок «как в S1…S5»)
//
// Пускает только с токеном MOCKS_ADMIN_TOKEN и (с -allow-from) только с
// адресов заглушек в admin-сети: сервис Orders сюда не достучится.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type serveConfig struct {
	addr, task, paramsFile, results string
	orders, mocks                   string
	allowFrom, token                string
}

type server struct {
	cfg   serveConfig
	allow *allowList

	mu         sync.Mutex
	state      string // idle | running | done
	ids        []string
	waitS      float64
	current    string
	startedAt  time.Time
	finishedAt time.Time
	done       []scenarioResult
	out        *output
	results    map[string]any
	rep        *report
}

func serve(cfg serveConfig) int {
	if _, ok := tasks[cfg.task]; !ok {
		log.Printf("runner -serve: неизвестное задание %q", cfg.task)
		return 2
	}
	if _, err := loadParams(cfg.task, cfg.paramsFile); err != nil {
		log.Printf("runner -serve: параметры: %v", err)
		return 2
	}
	s := &server{cfg: cfg, state: "idle"}
	if cfg.allowFrom != "" {
		s.allow = &allowList{name: cfg.allowFrom}
	}
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("runner -serve: слушаю %s", cfg.addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("runner -serve: %v", err)
	}
	return 2
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", s.guard(s.handleRun))
	mux.HandleFunc("GET /status", s.guard(s.handleStatus))
	mux.HandleFunc("GET /report", s.guard(s.handleReport))
	mux.HandleFunc("GET /params", s.guard(s.handleParams))
	return mux
}

// guard — токен и адрес отправителя.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.allow != nil && !s.allow.allowed(r.RemoteAddr) {
			writeJSONResp(w, http.StatusForbidden, apiErr("forbidden", "только через заглушки (дашборд)"))
			return
		}
		if s.cfg.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(s.cfg.token)) != 1 {
			writeJSONResp(w, http.StatusForbidden, apiErr("forbidden", "нужен заголовок X-Admin-Token"))
			return
		}
		h(w, r)
	}
}

type runRequest struct {
	Only string   `json:"only"`
	Wait *float64 `json:"wait"`
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONResp(w, http.StatusBadRequest, apiErr("bad_request", `ожидается {"only": "S1,S5", "wait": 30}`))
		return
	}
	params, err := loadParams(s.cfg.task, s.cfg.paramsFile)
	if err != nil {
		writeJSONResp(w, http.StatusInternalServerError, apiErr("params", err.Error()))
		return
	}
	ids := tasks[s.cfg.task]
	if req.Only != "" {
		ids = splitOnly(req.Only)
	}
	if len(ids) == 0 {
		writeJSONResp(w, http.StatusBadRequest, apiErr("bad_request", "нет сценариев"))
		return
	}
	if bad := unknownID(params, ids); bad != "" {
		writeJSONResp(w, http.StatusBadRequest, apiErr("bad_request", fmt.Sprintf("неизвестный сценарий %q", clean(bad))))
		return
	}
	if req.Wait != nil {
		if !validWait(*req.Wait) {
			writeJSONResp(w, http.StatusBadRequest, apiErr("bad_request", "wait: ожидается число секунд 0…3600"))
			return
		}
		params.WaitS = *req.Wait
	}

	s.mu.Lock()
	if s.state == "running" {
		s.mu.Unlock()
		writeJSONResp(w, http.StatusConflict, apiErr("busy", "прогон уже идёт"))
		return
	}
	out := newOutput(os.Stdout)
	rn := newRunner(params, s.cfg.orders, s.cfg.mocks, s.cfg.token, out)
	rn.progress = func(current string, done []scenarioResult) {
		s.mu.Lock()
		s.current, s.done = current, done
		s.mu.Unlock()
	}
	s.state, s.ids, s.waitS, s.current = "running", ids, params.WaitS, ""
	s.startedAt, s.finishedAt = time.Now(), time.Time{}
	s.done, s.out, s.results, s.rep = nil, out, nil, nil
	st := s.statusLocked()
	s.mu.Unlock()

	go s.execute(rn, ids)
	writeJSONResp(w, http.StatusAccepted, st)
}

func (s *server) execute(rn *runner, ids []string) {
	defer func() {
		if p := recover(); p != nil {
			rn.out.printf("СТЕНД: прогон прерван: %v\n", p)
		}
		s.mu.Lock()
		s.state, s.current, s.finishedAt = "done", "", time.Now()
		s.results, s.rep = rn.lastResults, rn.lastReport
		s.mu.Unlock()
	}()
	rn.run(s.cfg.task, ids, s.cfg.results)
}

type scenarioStatus struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"` // pending | running | pass | fail
	Error string `json:"error,omitempty"`
}

type runStatus struct {
	State      string           `json:"state"`
	Current    string           `json:"current,omitempty"`
	StartedAt  *time.Time       `json:"started_at,omitempty"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	WaitS      float64          `json:"wait_s"`
	Scenarios  []scenarioStatus `json:"scenarios"`
	Text       string           `json:"text"`
	Results    map[string]any   `json:"results"`
	Level      string           `json:"level,omitempty"`
	Passed     int              `json:"passed"`
	Total      int              `json:"total"`
	Report     bool             `json:"report"`
}

func (s *server) statusLocked() runStatus {
	st := runStatus{State: s.state, Current: s.current, WaitS: s.waitS, Scenarios: []scenarioStatus{},
		Results: s.results, Total: len(s.ids), Report: s.rep != nil}
	if !s.startedAt.IsZero() {
		t := s.startedAt
		st.StartedAt = &t
	}
	if !s.finishedAt.IsZero() {
		t := s.finishedAt
		st.FinishedAt = &t
	}
	if s.out != nil {
		st.Text = s.out.text()
	}
	byID := map[string]scenarioResult{}
	for _, res := range s.done {
		byID[res.ID] = res
	}
	for _, id := range s.ids {
		ss := scenarioStatus{ID: id, State: "pending"}
		if def := scenarioByID(id); def != nil {
			ss.Title = def.Title
		}
		if res, ok := byID[id]; ok {
			ss.State, ss.Error = "fail", res.Error
			if res.Pass {
				ss.State = "pass"
				st.Passed++
			}
		} else if id == s.current && s.state == "running" {
			ss.State = "running"
		}
		st.Scenarios = append(st.Scenarios, ss)
	}
	if s.results != nil {
		if lvl, ok := s.results["level"].(string); ok {
			st.Level = lvl
		}
	}
	return st
}

func (s *server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	st := s.statusLocked()
	s.mu.Unlock()
	writeJSONResp(w, http.StatusOK, st)
}

func (s *server) handleReport(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	rep := s.rep
	s.mu.Unlock()
	if rep == nil {
		writeJSONResp(w, http.StatusNotFound, apiErr("no_report", "отчёта ещё нет: прогон не запускался или не закончился"))
		return
	}
	var b bytes.Buffer
	if err := renderReport(&b, rep); err != nil {
		writeJSONResp(w, http.StatusInternalServerError, apiErr("report", err.Error()))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b.Bytes())
}

// publicScenario — открытые параметры сценария для кнопок «как в S1…S5».
type publicScenario struct {
	Title         string       `json:"title"`
	Orders        int          `json:"orders"`
	RatePerS      float64      `json:"rate_per_s"`
	Payment       PaymentModes `json:"payment"`
	OrdersAfter   int          `json:"orders_after,omitempty"`
	AfterRatePerS float64      `json:"after_rate_per_s,omitempty"`
}

func (s *server) handleParams(w http.ResponseWriter, _ *http.Request) {
	p, err := loadParams(s.cfg.task, s.cfg.paramsFile)
	if err != nil {
		writeJSONResp(w, http.StatusInternalServerError, apiErr("params", err.Error()))
		return
	}
	out := map[string]publicScenario{}
	for _, id := range tasks[s.cfg.task] {
		sp, def := p.scenario(id), scenarioByID(id)
		pm := sp.Payment
		if id == "S4" {
			pm.BlackholeAfterS = sp.DownAfterS
		}
		out[id] = publicScenario{Title: def.Title, Orders: sp.Orders, RatePerS: sp.RatePerS, Payment: pm,
			OrdersAfter: sp.OrdersAfter, AfterRatePerS: sp.AfterRatePerS}
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"wait_s": p.WaitS, "budget_ms": p.BudgetMS, "scenarios": out})
}

// allowList — адреса, которым можно: loopback и адреса имени name (в
// compose — mocks-admin, имя есть только в admin-сети). Кэш на 5 с.
type allowList struct {
	name string

	mu       sync.Mutex
	ips      map[string]bool
	until    time.Time
	resolved time.Time
}

func (a *allowList) allowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Чужой адрес не заставляет резолвить имя на каждый запрос: не чаще раза в секунду.
	if now := time.Now(); now.After(a.until) || (!a.ips[ip.String()] && now.Sub(a.resolved) > time.Second) {
		a.resolved = now
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(ctx, a.name)
		cancel()
		if err == nil {
			a.ips = map[string]bool{}
			for _, s := range addrs {
				if p := net.ParseIP(s); p != nil {
					a.ips[p.String()] = true
				}
			}
			a.until = time.Now().Add(5 * time.Second)
		}
	}
	return a.ips[ip.String()]
}

func apiErr(code, msg string) map[string]string {
	return map[string]string{"error": code, "message": msg}
}

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
