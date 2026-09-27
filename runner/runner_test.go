package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tOrder — заказ синтетического прогона: что увидел раннер и что в фактах.
type tOrder struct {
	id       string // "" — раннер id не получил
	status   string // итог GET
	getAmt   int64  // amount_cents из GET; 0 — как в запросе
	charges  int
	charged  int64 // сумма списаний; 0 — charges × сумма запроса
	resv     string
	declines int
	phase    string
	latency  time.Duration
	notSent  bool // заказ не из POST раннера (виден только в фактах)
}

func testParams(t *testing.T) *Params {
	t.Helper()
	p, err := loadParams("1", "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mkState(t *testing.T, orders []tOrder) *runState {
	t.Helper()
	f := &Facts{}
	f.Payment.Requests.ByOrder = map[string]int{}
	f.Payment.Requests.ByStatus = map[string]int{}
	f.Payment.Charges.ByOrder = map[string]int{}
	f.Payment.Charges.AmountByOrder = map[string]int64{}
	f.Payment.Declines.ByOrder = map[string]int{}
	f.Inventory.Reservations.ByOrder = map[string]string{}
	rs := &runState{params: testParams(t), facts: f, final: map[string]string{},
		amounts: map[string]int64{}, prev: map[string]bool{}}
	for i, o := range orders {
		s := &sent{N: i + 1, Phase: o.phase, Latency: o.latency, Code: 201, ID: o.id, Status: o.status,
			Req: orderReq{UserID: "u-1", Items: []item{{SKU: "sku-1", Qty: 2, PriceCents: 1990}}}}
		if o.latency == 0 {
			s.Latency = 100 * time.Millisecond
		}
		want := s.expected()
		if !o.notSent {
			rs.orders = append(rs.orders, s)
		}
		if o.id == "" {
			continue
		}
		rs.final[o.id] = o.status
		rs.amounts[o.id] = want
		if o.getAmt != 0 {
			rs.amounts[o.id] = o.getAmt
		}
		if o.charges > 0 {
			f.Payment.Charges.ByOrder[o.id] = o.charges
			f.Payment.Charges.Total += o.charges
			f.Payment.Charges.AmountByOrder[o.id] = int64(o.charges) * want
			if o.charged != 0 {
				f.Payment.Charges.AmountByOrder[o.id] = o.charged
			}
			f.Payment.Requests.ByOrder[o.id] += o.charges
		}
		if o.declines > 0 {
			f.Payment.Declines.ByOrder[o.id] = o.declines
			f.Payment.Requests.ByOrder[o.id] += o.declines
		}
		if o.resv != "" {
			f.Inventory.Reservations.ByOrder[o.id] = o.resv
		}
	}
	return rs
}

func findCheck(t *testing.T, cs []check, prefix string) check {
	t.Helper()
	for _, c := range cs {
		if strings.HasPrefix(c.Name, prefix) {
			return c
		}
	}
	t.Fatalf("нет проверки %q", prefix)
	return check{}
}

func failed(cs []check) []string {
	var out []string
	for _, c := range cs {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func paidOK(id string) tOrder { return tOrder{id: id, status: "paid", charges: 1, resv: "held"} }
func declined(id string) tOrder {
	return tOrder{id: id, status: "rejected", declines: 1, resv: "released"}
}

func TestCommonChecksClean(t *testing.T) {
	rs := mkState(t, []tOrder{paidOK("a"), paidOK("b"), declined("c")})
	if bad := failed(commonChecks(rs.params, rs)); len(bad) != 0 {
		t.Fatalf("провалы на чистом прогоне: %v", bad)
	}
}

func TestCommonChecksFailures(t *testing.T) {
	cases := []struct {
		name  string
		order tOrder
		check string
	}{
		{"двойное списание", tOrder{id: "x", status: "paid", charges: 2, resv: "held"}, "ни одного заказа с двумя списаниями"},
		{"rejected с деньгами", tOrder{id: "x", status: "rejected", charges: 1, resv: "released"}, "ни одного rejected, за который"},
		{"деньги без paid", tOrder{id: "x", status: "pending", charges: 1, resv: "held"}, "каждый заказ, за который списаны деньги"},
		{"деньги без заказа", tOrder{id: "x", status: "404", charges: 1, notSent: true}, "каждый заказ, за который списаны деньги"},
		{"paid без резерва", tOrder{id: "x", status: "paid", charges: 1}, "paid: деньги списаны"},
		{"paid без списания", tOrder{id: "x", status: "paid", resv: "held"}, "paid: деньги списаны"},
		{"неверная сумма списания", tOrder{id: "x", status: "paid", charges: 1, charged: 100, resv: "held"}, "списана сумма заказа"},
		{"неверная сумма в GET", tOrder{id: "x", status: "paid", charges: 1, getAmt: 100, resv: "held"}, "списана сумма заказа"},
		{"rejected с резервом", tOrder{id: "x", status: "rejected", declines: 1, resv: "held"}, "rejected: резерв снят"},
		{"pending в конце", tOrder{id: "x", status: "pending", resv: "held"}, "в конце прогона нет pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := mkState(t, []tOrder{paidOK("a"), tc.order})
			cs := commonChecks(rs.params, rs)
			c := findCheck(t, cs, tc.check)
			if c.OK || len(c.Orders) != 1 || c.Orders[0] != "x" {
				t.Fatalf("%+v", c)
			}
			if tc.name == "двойное списание" && c.kind != kindDouble {
				t.Fatal("двойное списание не влияет на уровень")
			}
			// Двойное списание не дублируется в проверке суммы.
			if tc.name == "двойное списание" && !findCheck(t, cs, "списана сумма заказа").OK {
				t.Fatal("сумма провалена из-за двойного списания")
			}
		})
	}
}

func TestBudget(t *testing.T) {
	rs := mkState(t, []tOrder{paidOK("a"), {id: "b", status: "paid", charges: 1, resv: "held", latency: 2 * time.Second}})
	c := findCheck(t, commonChecks(rs.params, rs), "каждый ответ на POST /orders")
	if c.OK || c.kind != kindBudget {
		t.Fatalf("%+v", c)
	}
}

func TestPrevScenarioExcluded(t *testing.T) {
	rs := mkState(t, []tOrder{paidOK("a"), {id: "old", status: "pending", charges: 2, notSent: true}})
	rs.prev["old"] = true
	delete(rs.final, "old")
	rs.facts.Payment.Requests.Log = []payLog{{OrderID: "old", Outcome: "201"}, {OrderID: "a", Outcome: "201"}}
	rs.facts.Payment.GetRequests.Log = []payLog{{OrderID: "old", Outcome: "200"}}
	for _, id := range rs.allIDs() {
		if id == "old" {
			t.Fatal("заказ прошлого сценария в allIDs")
		}
	}
	if bad := failed(commonChecks(rs.params, rs)); len(bad) != 0 {
		t.Fatalf("заказ прошлого сценария проверяется: %v", bad)
	}
	if n := rs.prevRequests(); n != 2 {
		t.Fatalf("prevRequests = %d", n)
	}
	j := strings.Join(journal(rs, scenarioByID("S1")), "\n")
	if !strings.Contains(j, "запросы к Payment по заказам прошлых сценариев: 2") {
		t.Fatalf("журнал:\n%s", j)
	}
	if strings.Contains(j, "2+ списаниями: 1") {
		t.Fatalf("двойное списание прошлого сценария в журнале:\n%s", j)
	}
}

func TestJournalNoHints(t *testing.T) {
	rs := mkState(t, []tOrder{paidOK("a"), {id: "ghost", status: "404", charges: 1, notSent: true},
		{id: "lost", status: "paid", charges: 1, resv: "held", notSent: true}})
	rs.facts.Payment.Requests.Log = []payLog{{OrderID: "a", Outcome: "201"}}
	rs.facts.Payment.GetRequests.Total = 3
	j := strings.Join(journal(rs, scenarioByID("S4")), "\n")
	for _, w := range []string{"ключ", "lookup", "тайм", "повтор", "идемпот"} {
		if strings.Contains(strings.ToLower(j), w) {
			t.Fatalf("в журнале %q:\n%s", w, j)
		}
	}
	for _, w := range []string{
		"списания по заказам, которых Orders не знает (GET 404): 1",
		"чей id раннер не получил в ответе на POST: 1",
		"запросов к Payment за время отказа: 0",
	} {
		if !strings.Contains(j, w) {
			t.Fatalf("нет %q:\n%s", w, j)
		}
	}
}

func TestS1(t *testing.T) {
	rs := mkState(t, []tOrder{paidOK("a"), declined("b")})
	if bad := failed(checksS1(nil, rs)); len(bad) != 0 {
		t.Fatal(bad)
	}
	rs = mkState(t, []tOrder{paidOK("a"), {id: "b", status: "rejected", resv: "released"}})
	c := findCheck(t, checksS1(nil, rs), "rejected — только когда банк")
	if c.OK {
		t.Fatal("rejected без отказа банка прошёл")
	}
}

func TestS2LegitDeclinesExcluded(t *testing.T) {
	p := &testParams(t).Scenarios.S2
	orders := []tOrder{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		orders = append(orders, paidOK(id))
	}
	orders = append(orders, declined("d1"), declined("d2"), declined("d3"))
	rs := mkState(t, orders)
	c := checksS2(p, rs)[0]
	if !c.OK || !strings.Contains(c.Detail, "9 из 9") {
		t.Fatalf("%+v", c)
	}
	// rejected без отказа банка — в знаменателе.
	orders = append(orders, tOrder{id: "r1", status: "rejected", resv: "released"},
		tOrder{id: "r2", status: "rejected", resv: "released"})
	rs = mkState(t, orders)
	if c := checksS2(p, rs)[0]; c.OK {
		t.Fatalf("%+v", c)
	}
}

func TestS4(t *testing.T) {
	p := &testParams(t).Scenarios.S4
	var orders []tOrder
	for _, id := range []string{"a1", "a2"} {
		o := paidOK(id)
		o.phase = "after"
		orders = append(orders, o)
	}
	d := declined("a3")
	d.phase = "after"
	orders = append(orders, d)
	rs := mkState(t, orders)
	rs.prev["old"] = true
	var post, look []payLog
	for range p.MaxRequestsDown - 5 {
		post = append(post, payLog{OrderID: "x", Blackhole: true, Outcome: "blackhole"})
	}
	for range 5 {
		look = append(look, payLog{OrderID: "x", Blackhole: true, Outcome: "blackhole"})
	}
	// Запросы по заказу прошлого сценария и вне окна не считаются.
	post = append(post, payLog{OrderID: "old", Blackhole: true}, payLog{OrderID: "x", Outcome: "201"})
	rs.facts.Payment.Requests.Log, rs.facts.Payment.GetRequests.Log = post, look
	cs := checksS4(p, rs)
	if !cs[0].OK || cs[0].Detail != "запросов к Payment за время отказа: 30" || strings.Contains(cs[0].Name, "lookup") {
		t.Fatalf("%+v", cs[0])
	}
	if !cs[1].OK || !strings.Contains(cs[1].Name, "после подъёма") {
		t.Fatalf("%+v", cs[1])
	}
	rs.facts.Payment.GetRequests.Log = append(look, payLog{OrderID: "y", Blackhole: true})
	if cs := checksS4(p, rs); cs[0].OK {
		t.Fatalf("порог не сработал: %+v", cs[0])
	}
	rs.final["a2"] = "pending"
	if cs := checksS4(p, rs); cs[1].OK || !strings.Contains(cs[1].Detail, "1 из 2") {
		t.Fatalf("%+v", cs[1])
	}
}

func TestLevel(t *testing.T) {
	mk := func(pass map[string]bool, double, budget bool) []scenarioResult {
		var out []scenarioResult
		for _, id := range tasks["1"] {
			out = append(out, scenarioResult{ID: id, Pass: pass[id], coreOK: !double, budgetOK: !budget})
		}
		return out
	}
	all := map[string]bool{"S1": true, "S2": true, "S3": true, "S4": true, "S5": true}
	onlyS1 := map[string]bool{"S1": true}
	ids := tasks["1"]
	cases := []struct {
		res  []scenarioResult
		want string
	}{
		{mk(all, false, false), "полностью"},
		{mk(onlyS1, false, false), "засчитано"},
		{mk(onlyS1, true, false), "не засчитано"},
		{mk(onlyS1, false, true), "не засчитано"},
		{mk(map[string]bool{"S2": true}, false, false), "не засчитано"},
	}
	for i, c := range cases {
		if got, line := level("1", ids, c.res); got != c.want || !strings.HasPrefix(line, "Уровень: "+c.want) {
			t.Fatalf("#%d: %q %q", i, got, line)
		}
	}
	if got, _ := level("1", []string{"S1"}, mk(all, false, false)[:1]); got != "" {
		t.Fatalf("частичный прогон: %q", got)
	}
}

func TestEvaluateSanitizes(t *testing.T) {
	evil := "x\n::error::bad ```" + strings.Repeat("z", 100)
	rs := mkState(t, []tOrder{paidOK("a"), {id: evil, status: "pending", resv: "held"}})
	var res scenarioResult
	evaluate(&res, scenarioByID("S3"), &rs.params.Scenarios.S3, rs)
	if res.Pass {
		t.Fatal("pending прошёл")
	}
	o := newOutput(&strings.Builder{})
	printResult(o, res)
	for _, line := range strings.Split(o.buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "::") || strings.Contains(line, "`") {
			t.Fatalf("инъекция в выводе: %q", line)
		}
	}
	for _, c := range res.Checks {
		for _, id := range c.Orders {
			if strings.ContainsAny(id, "\n`") || len([]rune(id)) > 64 {
				t.Fatalf("id в JSON: %q", id)
			}
		}
	}
	if s := short("заказ с пробелами и очень длинный"); len([]rune(s)) > 12 {
		t.Fatalf("short: %q", s)
	}
	if s := trim("a\r\nb\tc\x00d`e\u202e", 80); s != "a  b c d?e?" {
		t.Fatalf("trim: %q", s)
	}
}

func TestParamsValidate(t *testing.T) {
	p := testParams(t)
	if p.Scenarios.S4.DownAfterS != 0 {
		t.Fatal("down_after_s")
	}
	bad := []func(p *Params){
		func(p *Params) { p.Scenarios.S4.Orders = 200 }, // 200/2 = 100 с ≥ 60 с
		func(p *Params) { p.Scenarios.S2.MinPaid = 1.5 },
		func(p *Params) { p.Scenarios.S1.Payment.DeclineRate = -0.1 },
		func(p *Params) { p.Scenarios.S3.RatePerS = 0 },
		func(p *Params) { p.Scenarios.S5.Payment.LostResponse = "rate:2" },
		func(p *Params) { p.Scenarios.S4.Payment.BlackholeS = 1e12 },
		func(p *Params) { p.Scenarios.S4.Payment.BlackholeAfterS = 5 },
		func(p *Params) { p.BudgetMS = 0 },
	}
	for i, f := range bad {
		q := testParams(t)
		f(q)
		if q.validate("1") == nil {
			t.Fatalf("#%d принят", i)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "p.json")
	os.WriteFile(path, []byte(`{"scenarios":{"S4":{"down_after_s":10}}}`), 0o644)
	if q, err := loadParams("1", path); err != nil || q.Scenarios.S4.DownAfterS != 10 || q.Scenarios.S4.Orders != 50 {
		t.Fatalf("%v %+v", err, q)
	}
}

func TestWriteResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task_1.json")
	o := newOutput(&strings.Builder{})
	o.printf("строка\x1b[31m`\n")
	writeResults(path, map[string]any{"task": "1", "error": "x", "passed": 0, "total": 5,
		"scenarios": []scenarioResult{}, "level": ""}, o, &report{Task: "1", Error: "x", Total: 5})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["total"].(float64) != 5 {
		t.Fatalf("%v %s", err, b)
	}
	txt, err := os.ReadFile(filepath.Join(filepath.Dir(path), "task_1.txt"))
	if err != nil || string(txt) != "строка [31m?\n" {
		t.Fatalf("%v %q", err, txt)
	}
}
