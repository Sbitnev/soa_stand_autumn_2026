// Раннер сценариев: гоняет поток заказов в Orders, ломает заглушки через
// /admin/*, после паузы сверяет статусы заказов с журналом фактов заглушек.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed params/*.json
var paramFiles embed.FS

// Params — параметры сценариев. Файл подмены (-params) накладывается поверх
// встроенного: переопределяются только присутствующие в нём поля.
type Params struct {
	Seed             uint64  `json:"seed"`
	BudgetMS         int     `json:"budget_ms"`
	WaitS            float64 `json:"wait_s"`
	RequestTimeoutMS int     `json:"request_timeout_ms"`
	Scenarios        struct {
		S1 ScenarioParams `json:"S1"`
		S2 ScenarioParams `json:"S2"`
		S3 ScenarioParams `json:"S3"`
		S4 ScenarioParams `json:"S4"`
		S5 ScenarioParams `json:"S5"`
	} `json:"scenarios"`
}

type ScenarioParams struct {
	Orders   int          `json:"orders"`
	RatePerS float64      `json:"rate_per_s"`
	Payment  PaymentModes `json:"payment"`
	// S2: минимальная доля оплаченных заказов.
	MinPaid float64 `json:"min_paid,omitempty"`
	// S4: через сколько секунд после установки режимов Payment перестаёт
	// отвечать, заказы после подъёма Payment и предел запросов за время отказа.
	DownAfterS      float64 `json:"down_after_s,omitempty"`
	OrdersAfter     int     `json:"orders_after,omitempty"`
	AfterRatePerS   float64 `json:"after_rate_per_s,omitempty"`
	MaxRequestsDown int     `json:"max_requests_down,omitempty"`
}

type PaymentModes struct {
	FailRate    float64 `json:"fail_rate,omitempty"`
	DeclineRate float64 `json:"decline_rate,omitempty"`
	LatencyMS   int     `json:"latency_ms,omitempty"`
	BlackholeS  float64 `json:"blackhole_s,omitempty"`
	// BlackholeAfterS в файле параметров не задаётся: раннер берёт его из
	// down_after_s сценария.
	BlackholeAfterS float64 `json:"blackhole_after_s,omitempty"`
	LostResponse    string  `json:"lost_response,omitempty"`
}

func (p *Params) scenario(id string) *ScenarioParams {
	switch id {
	case "S1":
		return &p.Scenarios.S1
	case "S2":
		return &p.Scenarios.S2
	case "S3":
		return &p.Scenarios.S3
	case "S4":
		return &p.Scenarios.S4
	case "S5":
		return &p.Scenarios.S5
	}
	return nil
}

var tasks = map[string][]string{
	"1": {"S1", "S2", "S3", "S4", "S5"},
}

func main() {
	var (
		task       = flag.String("task", envOr("TASK", "1"), "номер задания")
		only       = flag.String("only", os.Getenv("ONLY"), "только эти сценарии, через запятую: S3 или S2,S5")
		paramsFile = flag.String("params", os.Getenv("PARAMS"), "файл подмены параметров (JSON)")
		wait       = flag.String("wait", os.Getenv("WAIT"), "пауза после последнего заказа, с (по умолчанию из параметров)")
		ordersURL  = flag.String("orders-url", envOr("ORDERS_URL", "http://orders:8080"), "адрес Orders")
		mocksURL   = flag.String("mocks-url", envOr("MOCKS_URL", "http://mocks:8080"), "адрес заглушек")
		results    = flag.String("results", os.Getenv("RESULTS"), "куда записать результаты в JSON")
		serveAddr  = flag.String("serve", "", "режим сервиса для дашборда: слушать адрес (:8081) и запускать прогоны по POST /run")
		allowFrom  = flag.String("allow-from", os.Getenv("RUNNER_ALLOW_FROM"), "-serve: пускать только с адресов этого имени (mocks-admin)")
	)
	flag.Parse()

	if *serveAddr != "" {
		os.Exit(serve(serveConfig{
			addr: *serveAddr, task: *task, paramsFile: *paramsFile, results: *results,
			orders: *ordersURL, mocks: *mocksURL, allowFrom: *allowFrom,
			token: os.Getenv("MOCKS_ADMIN_TOKEN"),
		}))
	}

	ids, ok := tasks[*task]
	if !ok {
		failRun(*results, *task, 0, fmt.Sprintf("неизвестное задание %q", *task))
	}
	if *only != "" {
		ids = splitOnly(*only)
	}

	params, err := loadParams(*task, *paramsFile)
	if err != nil {
		failRun(*results, *task, len(ids), fmt.Sprintf("параметры: %v", err))
	}
	if bad := unknownID(params, ids); bad != "" {
		failRun(*results, *task, len(ids), fmt.Sprintf("неизвестный сценарий %q", bad))
	}
	if *wait != "" {
		v, err := strconv.ParseFloat(*wait, 64)
		if err != nil || !validWait(v) {
			failRun(*results, *task, len(ids), "WAIT: ожидается число секунд 0…3600")
		}
		params.WaitS = v
	}

	r := newRunner(params, *ordersURL, *mocksURL, os.Getenv("MOCKS_ADMIN_TOKEN"), newOutput(os.Stdout))
	// Прогон из дашборда (make ui) ходит в те же заглушки: два прогона
	// сразу испортили бы друг другу проверку.
	if r.dashboardBusy() {
		fmt.Println("СТЕНД: сейчас идёт прогон сценариев из дашборда (make ui): оба прогона ходят в одни и те же заглушки.")
		fmt.Println("Дождитесь его конца на дашборде и запустите make scenarios снова.")
		os.Exit(2)
	}
	os.Exit(r.run(*task, ids, *results))
}

// splitOnly — список сценариев из -only: «s2, S5» → [S2 S5].
func splitOnly(only string) []string {
	var ids []string
	for _, s := range strings.Split(only, ",") {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// unknownID — первый сценарий, которого нет, или "".
func unknownID(p *Params, ids []string) string {
	for _, id := range ids {
		if p.scenario(id) == nil || scenarioByID(id) == nil {
			return id
		}
	}
	return ""
}

func validWait(v float64) bool { return v >= 0 && v <= maxS }

func newRunner(params *Params, ordersURL, mocksURL, token string, out *output) *runner {
	return &runner{
		out:        out,
		prev:       map[string]bool{},
		adminToken: token,
		params:     params,
		orders:     strings.TrimRight(ordersURL, "/"),
		mocks:      strings.TrimRight(mocksURL, "/"),
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			IdleConnTimeout:     30 * time.Second,
		}},
	}
}

// dashboardBusy — идёт ли прогон из дашборда (через прокси заглушек к
// runner-ui). Любая ошибка — «не идёт»: runner-ui обычно не поднят.
func (r *runner) dashboardBusy() bool {
	code, b, err := r.do(http.MethodGet, r.mocks+"/admin/runner/status", nil, 4*time.Second)
	if err != nil || code != http.StatusOK {
		return false
	}
	var st struct {
		State string `json:"state"`
	}
	return json.Unmarshal(b, &st) == nil && st.State == "running"
}

func loadParams(task, override string) (*Params, error) {
	var p Params
	b, err := paramFiles.ReadFile("params/task" + task + ".json")
	if err != nil {
		return nil, err
	}
	if err := strictUnmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("встроенный файл: %w", err)
	}
	if override != "" {
		b, err := os.ReadFile(override)
		if err != nil {
			return nil, err
		}
		if err := strictUnmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", override, err)
		}
	}
	if err := p.validate(task); err != nil {
		return nil, err
	}
	return &p, nil
}

// validate — числа в параметрах осмысленны, иначе прогон бессмыслен.
func (p *Params) validate(task string) error {
	switch {
	case p.BudgetMS <= 0:
		return fmt.Errorf("budget_ms: ожидается > 0")
	case p.RequestTimeoutMS <= 0:
		return fmt.Errorf("request_timeout_ms: ожидается > 0")
	case !(p.WaitS >= 0 && p.WaitS <= maxS):
		return fmt.Errorf("wait_s: ожидается 0…%d", maxS)
	}
	for _, id := range tasks[task] {
		if err := p.scenario(id).validate(id); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

// maxS — предел длительностей в параметрах, с.
const maxS = 3600

func (sp *ScenarioParams) validate(id string) error {
	frac := func(name string, v float64) error {
		if !(v >= 0 && v <= 1) {
			return fmt.Errorf("%s: ожидается доля 0…1", name)
		}
		return nil
	}
	dur := func(name string, v float64) error {
		if !(v >= 0 && v <= maxS) {
			return fmt.Errorf("%s: ожидается 0…%d с", name, maxS)
		}
		return nil
	}
	pm := sp.Payment
	for _, err := range []error{
		frac("payment.fail_rate", pm.FailRate),
		frac("payment.decline_rate", pm.DeclineRate),
		frac("min_paid", sp.MinPaid),
		dur("payment.blackhole_s", pm.BlackholeS),
		dur("down_after_s", sp.DownAfterS),
	} {
		if err != nil {
			return err
		}
	}
	switch {
	case sp.Orders <= 0:
		return fmt.Errorf("orders: ожидается > 0")
	case !(sp.RatePerS > 0):
		return fmt.Errorf("rate_per_s: ожидается > 0")
	case pm.LatencyMS < 0 || pm.LatencyMS > maxS*1000:
		return fmt.Errorf("payment.latency_ms: ожидается 0…%d", maxS*1000)
	case pm.BlackholeAfterS != 0:
		return fmt.Errorf("payment.blackhole_after_s не задаётся: используйте down_after_s")
	case sp.OrdersAfter < 0 || sp.MaxRequestsDown < 0:
		return fmt.Errorf("orders_after и max_requests_down: ожидается ≥ 0")
	case sp.OrdersAfter > 0 && !(sp.AfterRatePerS > 0):
		return fmt.Errorf("after_rate_per_s: ожидается > 0")
	}
	if lr := pm.LostResponse; strings.HasPrefix(lr, "rate:") {
		v, err := strconv.ParseFloat(strings.TrimPrefix(lr, "rate:"), 64)
		if err != nil {
			return fmt.Errorf("payment.lost_response: rate:p, p — число")
		}
		if err := frac("payment.lost_response rate:p", v); err != nil {
			return err
		}
	}
	switch id {
	case "S2":
		if !(sp.MinPaid > 0) {
			return fmt.Errorf("min_paid: ожидается доля > 0")
		}
	case "S4":
		if !(pm.BlackholeS > 0) {
			return fmt.Errorf("payment.blackhole_s: ожидается > 0")
		}
		if send := float64(sp.Orders) / sp.RatePerS; !(send < pm.BlackholeS) {
			return fmt.Errorf("orders/rate_per_s = %.1f с: заказы во время отказа должны уйти быстрее payment.blackhole_s = %.1f с", send, pm.BlackholeS)
		}
		if sp.OrdersAfter <= 0 {
			return fmt.Errorf("orders_after: ожидается > 0")
		}
	}
	return nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type runner struct {
	out *output
	// prev — id заказов, известные по прошлым сценариям этого прогона: Orders
	// и его база между сценариями не пересоздаются, и работа Orders по старым
	// заказам может продолжаться в следующем сценарии.
	prev       map[string]bool
	adminToken string
	params     *Params
	orders     string
	mocks      string
	client     *http.Client

	// progress — ход прогона для runner -serve (nil в обычном режиме).
	progress func(current string, done []scenarioResult)
	// lastResults и lastReport — итог прогона: то же, что в results JSON и
	// HTML-отчёте.
	lastResults map[string]any
	lastReport  *report
}

// ---------- HTTP к заглушкам и Orders ----------

func (r *runner) do(method, url string, body any, timeout time.Duration) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.adminToken != "" && strings.HasPrefix(url, r.mocks+"/admin/") {
		req.Header.Set("X-Admin-Token", r.adminToken)
	}
	c := *r.client
	c.Timeout = timeout
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// Ответы Orders — не больше 1 МБ; факты заглушек бывают крупнее.
	limit := int64(1 << 20)
	if strings.HasPrefix(url, r.mocks+"/") {
		limit = 64 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, b, err
}

func (r *runner) admin(method, path string, body any) error {
	code, b, err := r.do(method, r.mocks+path, body, 5*time.Second)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, code, strings.TrimSpace(string(b)))
	}
	return nil
}

func (r *runner) waitHealthy(name, url string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		code, _, err := r.do(http.MethodGet, url+"/healthz", nil, 2*time.Second)
		if err == nil && code == http.StatusOK {
			return nil
		}
		if err != nil {
			last = shortErr(err)
		} else {
			last = fmt.Sprintf("ответ %d", code)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s не отвечает на GET /healthz за %v: %s", name, limit, last)
}

// payLog — один запрос к Payment в журнале заглушки.
type payLog struct {
	At          time.Time  `json:"at"`
	OrderID     string     `json:"order_id"`
	Outcome     string     `json:"outcome"`
	Blackhole   bool       `json:"blackhole"`
	Method      string     `json:"method"`
	AmountCents int64      `json:"amount_cents"`
	DoneAt      *time.Time `json:"done_at"`
	// ClientClosed — соединение закрыл клиент (Orders), не дождавшись ответа.
	ClientClosed bool `json:"client_closed"`
}

// invLog — один запрос к Inventory в журнале заглушки.
type invLog struct {
	At      time.Time `json:"at"`
	Method  string    `json:"method"`
	Op      string    `json:"op"`
	OrderID string    `json:"order_id"`
	Outcome string    `json:"outcome"`
}

type Facts struct {
	Payment struct {
		Requests struct {
			Total       int            `json:"total"`
			ByStatus    map[string]int `json:"by_status"`
			ByOrder     map[string]int `json:"by_order"`
			InBlackhole int            `json:"in_blackhole"`
			Log         []payLog       `json:"log"`
		} `json:"requests"`
		GetRequests struct {
			Total       int      `json:"total"`
			InBlackhole int      `json:"in_blackhole"`
			Log         []payLog `json:"log"`
		} `json:"get_requests"`
		Charges struct {
			Total         int              `json:"total"`
			ByOrder       map[string]int   `json:"by_order"`
			AmountByOrder map[string]int64 `json:"amount_by_order"`
		} `json:"charges"`
		Declines struct {
			Total   int            `json:"total"`
			ByOrder map[string]int `json:"by_order"`
		} `json:"declines"`
	} `json:"payment"`
	Inventory struct {
		Requests struct {
			Reserve int `json:"reserve"`
			Release int `json:"release"`
		} `json:"requests"`
		Log          []invLog `json:"log"`
		Reservations struct {
			Held     int               `json:"held"`
			Released int               `json:"released"`
			ByOrder  map[string]string `json:"by_order"`
		} `json:"reservations"`
	} `json:"inventory"`
}

func (r *runner) facts() (*Facts, error) {
	code, b, err := r.do(http.MethodGet, r.mocks+"/admin/facts", nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET /admin/facts: %d", code)
	}
	var f Facts
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// failRun — прогон не начался: пишет результаты с ошибкой (если задан путь)
// и выходит с кодом 2.
func failRun(resultsPath, task string, total int, msg string) {
	o := newOutput(os.Stdout)
	o.printf("СТЕНД: %s\n", msg)
	writeResults(resultsPath, map[string]any{
		"task": task, "error": clean(msg), "passed": 0, "total": total,
		"scenarios": []scenarioResult{}, "level": "",
	}, o, &report{Task: task, RunAt: time.Now(), Error: clean(msg), Total: total})
	os.Exit(2)
}
