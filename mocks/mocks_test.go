package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func setup(t *testing.T, m Modes) (*httptest.Server, *state) {
	t.Helper()
	st := newState(Modes{Seed: 1})
	st.mu.Lock()
	st.applyModesLocked(m)
	st.mu.Unlock()
	srv := httptest.NewServer(st.routes())
	t.Cleanup(srv.Close)
	return srv, st
}

func pay(t *testing.T, url, orderID, key string, amount int64) (int, error) {
	t.Helper()
	b, _ := json.Marshal(chargeRequest{OrderID: orderID, AmountCents: amount, Currency: "RUB"})
	req, _ := http.NewRequest(http.MethodPost, url+"/payments", bytes.NewReader(b))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func facts(t *testing.T, url string) Facts {
	t.Helper()
	resp, err := http.Get(url + "/admin/facts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var f Facts
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestKeyReplay(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1})
	if c, _ := pay(t, srv.URL, "o1", "k1", 100); c != 201 {
		t.Fatalf("first: %d", c)
	}
	if c, _ := pay(t, srv.URL, "o1", "k1", 100); c != 200 {
		t.Fatalf("replay: %d", c)
	}
	if c, _ := pay(t, srv.URL, "o1", "k1", 200); c != 422 {
		t.Fatalf("other body: %d", c)
	}
	if c, _ := pay(t, srv.URL, "o1", "", 100); c != 201 {
		t.Fatalf("no key: %d", c)
	}
	if got := facts(t, srv.URL).Payment.Charges.ByOrder["o1"]; got != 2 {
		t.Fatalf("charges = %d, want 2", got)
	}
}

func TestFailRateNoCharge(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 7, Payment: PaymentModes{FailRate: 1}})
	if c, _ := pay(t, srv.URL, "o1", "k", 100); c != 500 {
		t.Fatalf("code %d", c)
	}
	if f := facts(t, srv.URL); f.Payment.Charges.Total != 0 {
		t.Fatalf("charged on 500")
	}
}

func TestLostFirstPerOrder(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{LostResponse: "first_per_order"}})
	if _, err := pay(t, srv.URL, "o1", "k", 100); err == nil {
		t.Fatal("expected lost response")
	}
	if c, err := pay(t, srv.URL, "o1", "k", 100); err != nil || c != 200 {
		t.Fatalf("replay: %d %v", c, err)
	}
	f := facts(t, srv.URL)
	if f.Payment.Charges.ByOrder["o1"] != 1 || f.Payment.Requests.ByStatus["lost"] != 1 {
		t.Fatalf("facts %+v", f.Payment)
	}
}

func TestDeclineRemembered(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{DeclineRate: 1}})
	if c, _ := pay(t, srv.URL, "o1", "k", 100); c != 402 {
		t.Fatalf("code %d", c)
	}
	if c, _ := pay(t, srv.URL, "o1", "k", 100); c != 402 {
		t.Fatalf("replay code %d", c)
	}
}

func TestLatencyMode(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{LatencyMS: 1500}})
	b, _ := json.Marshal(chargeRequest{OrderID: "o1", AmountCents: 1, Currency: "RUB"})
	c := &http.Client{Timeout: 200 * time.Millisecond}
	if _, err := c.Post(srv.URL+"/payments", "application/json", bytes.NewReader(b)); err == nil {
		t.Fatal("expected client timeout")
	}
	if f := facts(t, srv.URL); f.Payment.Charges.ByOrder["o1"] != 1 {
		t.Fatalf("charges %v", f.Payment.Charges.ByOrder)
	}
}

func TestBlackholeWindow(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{BlackholeS: 0.5}})
	start := time.Now()
	if _, err := pay(t, srv.URL, "o1", "", 100); err == nil {
		t.Fatal("expected no response")
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("returned too fast: %v", d)
	}
	if c, err := pay(t, srv.URL, "o1", "", 100); err != nil || c != 201 {
		t.Fatalf("after window: %d %v", c, err)
	}
	f := facts(t, srv.URL)
	if f.Payment.Requests.InBlackhole != 1 || f.Payment.Charges.Total != 1 {
		t.Fatalf("facts %+v", f.Payment.Requests)
	}
}

func TestDeterministic(t *testing.T) {
	run := func() []int {
		srv, _ := setup(t, Modes{Seed: 42, Payment: PaymentModes{FailRate: 0.3, DeclineRate: 0.2}})
		var out []int
		for i := 0; i < 30; i++ {
			c, _ := pay(t, srv.URL, "o", "", 1)
			out = append(out, c)
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("differs at %d: %v vs %v", i, a, b)
		}
	}
}

func TestReservations(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1})
	body := `{"order_id":"o1","items":[{"sku":"sku-1","qty":1}]}`
	for i, want := range []int{201, 200} {
		resp, _ := http.Post(srv.URL+"/reservations", "application/json", bytes.NewBufferString(body))
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("#%d: %d", i, resp.StatusCode)
		}
	}
	for range 2 {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/reservations/o1", nil)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("delete: %d", resp.StatusCode)
		}
	}
	if f := facts(t, srv.URL); f.Inventory.Reservations.ByOrder["o1"] != "released" {
		t.Fatalf("%+v", f.Inventory)
	}
}

func TestAdminToken(t *testing.T) {
	st := newState(Modes{Seed: 1})
	st.adminToken = "secret"
	srv := httptest.NewServer(st.routes())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/admin/facts")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/facts", nil)
	req.Header.Set("X-Admin-Token", "secret")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: %d", resp.StatusCode)
	}
}

func TestDeclineByOrder(t *testing.T) {
	run := func() map[string]int {
		srv, _ := setup(t, Modes{Seed: 5, Payment: PaymentModes{DeclineRate: 0.5}})
		out := map[string]int{}
		for i := range 40 {
			id := fmt.Sprintf("o%d", i)
			c, _ := pay(t, srv.URL, id, "", 100)
			out[id] = c
		}
		// Повтор по тому же заказу без ключа получает тот же исход банка.
		for i := range 40 {
			id := fmt.Sprintf("o%d", i)
			c, _ := pay(t, srv.URL, id, "", 100)
			if (c == 402) != (out[id] == 402) {
				t.Fatalf("%s: first %d, again %d", id, out[id], c)
			}
		}
		return out
	}
	a, b := run(), run()
	declined := 0
	for id, c := range a {
		if (c == 402) != (b[id] == 402) {
			t.Fatalf("%s: %d vs %d between runs", id, c, b[id])
		}
		if c == 402 {
			declined++
		}
	}
	if declined == 0 || declined == len(a) {
		t.Fatalf("declined %d of %d", declined, len(a))
	}
}

func TestBlackholeAfter(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{BlackholeS: 0.5, BlackholeAfterS: 0.4}})
	if c, err := pay(t, srv.URL, "o1", "", 100); err != nil || c != 201 {
		t.Fatalf("before window: %d %v", c, err)
	}
	f := facts(t, srv.URL)
	if f.Payment.Blackhole == nil || f.Payment.Blackhole.Until.Sub(f.Payment.Blackhole.From) != 500*time.Millisecond {
		t.Fatalf("window %+v", f.Payment.Blackhole)
	}
	time.Sleep(time.Until(f.Payment.Blackhole.From) + 50*time.Millisecond)
	if _, err := pay(t, srv.URL, "o2", "", 100); err == nil {
		t.Fatal("expected no response inside window")
	}
	time.Sleep(time.Until(f.Payment.Blackhole.Until) + 50*time.Millisecond)
	if c, err := pay(t, srv.URL, "o3", "", 100); err != nil || c != 201 {
		t.Fatalf("after window: %d %v", c, err)
	}
	f = facts(t, srv.URL)
	if f.Payment.Requests.InBlackhole != 1 || f.Payment.Charges.Total != 2 {
		t.Fatalf("facts %+v", f.Payment.Requests)
	}
}

func TestBlackholeBadBodyHangs(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{BlackholeS: 0.3}})
	c := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := c.Post(srv.URL+"/payments", "application/json", bytes.NewBufferString("{кривое"))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got %d, expected no response", resp.StatusCode)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("returned too fast")
	}
}

func TestLookupInWindow(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1, Payment: PaymentModes{BlackholeS: 0.3}})
	c := &http.Client{Timeout: 3 * time.Second}
	if resp, err := c.Get(srv.URL + "/payments?order_id=o1"); err == nil {
		resp.Body.Close()
		t.Fatalf("lookup answered %d inside window", resp.StatusCode)
	}
	resp, err := c.Get(srv.URL + "/payments?order_id=o1")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("lookup after window: %v", err)
	}
	resp.Body.Close()
	f := facts(t, srv.URL)
	lr := f.Payment.GetRequests
	if f.Payment.GetRequests.Total != 2 || lr.InBlackhole != 1 || len(lr.Log) != 2 ||
		lr.Log[0].OrderID != "o1" || !lr.Log[0].Blackhole || lr.Log[1].Blackhole {
		t.Fatalf("lookups %d %+v", f.Payment.GetRequests.Total, lr)
	}
}

func TestAmountByOrder(t *testing.T) {
	srv, _ := setup(t, Modes{Seed: 1})
	pay(t, srv.URL, "o1", "", 150)
	pay(t, srv.URL, "o1", "", 150)
	pay(t, srv.URL, "o2", "", 990)
	a := facts(t, srv.URL).Payment.Charges.AmountByOrder
	if a["o1"] != 300 || a["o2"] != 990 {
		t.Fatalf("amount_by_order %v", a)
	}
}

func TestValidateLimits(t *testing.T) {
	for _, m := range []PaymentModes{
		{BlackholeS: 1e12}, {BlackholeAfterS: 3601}, {BlackholeS: -1}, {FailRate: 2}, {LatencyMS: -1},
	} {
		if m.validate() == nil {
			t.Fatalf("accepted %+v", m)
		}
	}
	if err := (PaymentModes{BlackholeS: 3600, BlackholeAfterS: 3600}).validate(); err != nil {
		t.Fatal(err)
	}
}
