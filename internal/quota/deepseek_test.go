package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeepSeekBalanceMatrix(t *testing.T) {
	ctx := context.Background()
	old := deepSeekBalanceURL
	defer func() { deepSeekBalanceURL = old }()

	mk := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	deepSeekBalanceURL = mk(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"110.35"},
		{"total_balance":0.5},
		{"currency":"USD"},
		{}
	]}`, http.StatusOK).URL
	info, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || !info.Available || info.Balance != "CNY 110.35 · 0.5" {
		t.Fatalf("balance = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":1}]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("unavailable = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"balance_infos":[]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("no infos = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"balance_infos":[{"currency":"CNY"}]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("no amounts = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk("", http.StatusTooManyRequests).URL
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected status error")
	}

	deepSeekBalanceURL = mk("garbage", http.StatusOK).URL
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected decode error")
	}

	deepSeekBalanceURL = "http://127.0.0.1:1/balance"
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected transport error")
	}
}
