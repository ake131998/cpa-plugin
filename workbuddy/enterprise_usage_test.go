package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// enterpriseBillingServer returns an httptest server that answers the
// enterprise usage endpoint with entBody and the personal get-user-resource
// endpoint with personalBody.
func enterpriseBillingServer(t *testing.T, entBody, personalBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/get-enterprise-user-usage":
			_, _ = w.Write([]byte(entBody))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(personalBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestFetchUserResource_EnterpriseUnlimited reproduces the 2026-08 production
// bug: enterprise accounts get an empty Accounts list from get-user-resource
// (panel usage card showed all zeros) while their real balance lives under
// get-enterprise-user-usage. limitNum=-1 means unlimited → size stays 0.
func TestFetchUserResource_EnterpriseUnlimited(t *testing.T) {
	srv := enterpriseBillingServer(t,
		`{"code":0,"msg":"OK","data":{"credit":94866.03,"limitNum":-1,"cycleStartTime":"2026-07-16 00:00:00","cycleEndTime":"2026-08-15 23:59:59","cycleResetTime":"2026-08-16 00:00:00"}}`,
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null},"RequestId":""}}}`,
	)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.EnterpriseID = "ent-1"
	sum, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if sum.TotalRemain != 94866 {
		t.Fatalf("remain=%d want 94866", sum.TotalRemain)
	}
	if sum.TotalSize != 0 || sum.TotalUsed != 0 {
		t.Fatalf("unlimited pool must keep size/used at 0, got size=%d used=%d", sum.TotalSize, sum.TotalUsed)
	}
	if len(sum.Packages) != 1 || sum.Packages[0].CycleEnd != "2026-08-15 23:59:59" {
		t.Fatalf("packages=%+v", sum.Packages)
	}
}

// TestFetchUserResource_EnterpriseFinitePool covers a positive limitNum:
// used = limit − remain.
func TestFetchUserResource_EnterpriseFinitePool(t *testing.T) {
	srv := enterpriseBillingServer(t,
		`{"code":0,"msg":"OK","data":{"credit":300,"limitNum":1000,"cycleStartTime":"2026-08-01 00:00:00","cycleEndTime":"2026-08-31 23:59:59"}}`,
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null},"RequestId":""}}}`,
	)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.EnterpriseID = "ent-1"
	sum, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if sum.TotalRemain != 300 || sum.TotalUsed != 700 || sum.TotalSize != 1000 {
		t.Fatalf("remain=%d used=%d size=%d, want 300/700/1000", sum.TotalRemain, sum.TotalUsed, sum.TotalSize)
	}
}

// TestFetchUserResource_EnterpriseFallsBackOnError: when the enterprise call
// fails, personal accounts behind the same credential still resolve via
// get-user-resource.
func TestFetchUserResource_EnterpriseFallsBackOnError(t *testing.T) {
	srv := enterpriseBillingServer(t,
		`{"code":500,"msg":"enterprise endpoint broken"}`,
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":500,"Accounts":[{"PackageName":"体验版","CapacityRemain":499,"CapacitySize":500,"CycleCapacityRemain":499,"CycleCapacitySize":500}]},"RequestId":""}}}`,
	)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.EnterpriseID = "ent-1"
	sum, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if sum.TotalRemain != 499 || sum.TotalUsed != 1 || sum.TotalSize != 500 {
		t.Fatalf("remain=%d used=%d size=%d, want 499/1/500", sum.TotalRemain, sum.TotalUsed, sum.TotalSize)
	}
}
