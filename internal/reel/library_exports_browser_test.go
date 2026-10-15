//go:build linux && browser

package reel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestBrowserExportsNativeFormsAndVerifiedDownload(t *testing.T) {
	base, state := libraryFixture(t)
	server := httptest.NewUnstartedServer(nil)
	base.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	exports, e := newLibraryExportsAccess(base.root, base.access)
	if e != nil {
		t.Fatal(e)
	}
	defer exports.Close()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/exports") {
			exports.ServeHTTP(w, r)
		} else {
			base.ServeHTTP(w, r)
		}
	})
	server.Start()
	defer server.Close()
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(server.URL+"/exports/new?recording="+state.ID), chromedp.WaitVisible(`form[action="/exports"]`))
	libraryBrowserScreenshot(t, ctx, "export-form")
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(`form[action="/exports"] button`)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(`a[href$="/download"]`))
	libraryBrowserScreenshot(t, ctx, "export-complete")
	var signature string
	runBrowser(t, ctx, chromedp.Evaluate(`(async()=>{const r=await fetch(document.querySelector('a[href$="/download"]').href); if(!r.ok)throw Error(r.status);const b=new Uint8Array(await r.arrayBuffer());return String.fromCharCode(...b.slice(0,4));})()`, &signature, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	if signature != "PK\x03\x04" {
		t.Fatalf("not a ZIP: %q", signature)
	}
}

func TestBrowserExportsReturnPreservesHistoricalCutoff(t *testing.T) {
	helper := os.Getenv("AREEL_TEST_AVT")
	if helper == "" {
		t.Fatal("history export browser qualification requires AREEL_TEST_AVT")
	}
	t.Setenv("AREEL_AVT_CHECKPOINT", helper)
	base, state := libraryFixture(t)
	defer base.close()
	work, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	selection, e := base.history.pin(work, base.root, state.ID, 1)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(nil)
	base.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	exports, e := newLibraryExportsAccess(base.root, base.access)
	if e != nil {
		t.Fatal(e)
	}
	exports.visual = base.history.exportPin
	defer exports.Close()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/exports") {
			exports.ServeHTTP(w, r)
		} else {
			base.ServeHTTP(w, r)
		}
	})
	server.Start()
	defer server.Close()
	ctx := libraryBrowser(t)
	checkReturn := func() {
		t.Helper()
		var href string
		runBrowser(t, ctx, chromedp.AttributeValue(`a[href^="/recordings/"]`, "href", &href, nil))
		u, e := url.Parse(href)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		if u.Path != "/recordings/"+state.ID || q.Get("mode") != "history" || q.Get("segment") != "1" || q.Get("cutoff") != selection.Token {
			t.Fatalf("return loses historical binding: %s", href)
		}
	}
	runBrowser(t, ctx, chromedp.Navigate(server.URL+"/exports/new?recording="+state.ID+"&segment=1&cutoff="+selection.Token), chromedp.WaitVisible(`form[action="/exports"]`))
	checkReturn()
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`form[action="/exports"] button`)); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(`a[href$="/download"]`))
	checkReturn()
	var downloaded bool
	runBrowser(t, ctx, chromedp.Evaluate(`(async()=>{const r=await fetch(document.querySelector('a[href$="/download"]').href);const b=await r.arrayBuffer();return r.ok&&b.byteLength>0;})()`, &downloaded, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	if !downloaded {
		t.Fatal("download failed")
	}
	checkReturn()
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`a[href^="/recordings/"]`)); e != nil {
		t.Fatal(e)
	}
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	u, e := url.Parse(location)
	if e != nil {
		t.Fatal(e)
	}
	if u.Query().Get("cutoff") != selection.Token || u.Query().Get("mode") != "history" || u.Query().Get("segment") != "1" {
		t.Fatal("navigation changed history", location)
	}
	var src string
	runBrowser(t, ctx, chromedp.AttributeValue(`#terminal-frame`, "data-src", &src, nil))
	if !strings.Contains(src, "cutoff="+selection.Token) {
		t.Fatal("renderer changed cutoff", src)
	}
	base.history.store.mu.Lock()
	count := len(base.history.store.cutoffs)
	base.history.store.mu.Unlock()
	if count != 1 {
		t.Fatalf("return repinned history: %d cutoffs", count)
	}
}
