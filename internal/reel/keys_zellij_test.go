//go:build (linux || darwin) && zellij

package reel

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Real Zellij (validated: 0.44.3, 0.45.1) in a throwaway private session (see throwawayZellij):
// the echo gate follows the focused pane of a real session through
// `zellij action list-clients` and /proc, and `areel attach` proxies a real
// `zellij attach` client, capturing its keys and masking a password read.
// Run with `sd areel zellij-check`.
func TestZellijKeysAttachAndGate(t *testing.T) {
	keysFastTimers(t)
	z, name := throwawayZellij(t)
	attachZellijClient(t, 100, 30, "--session", name)
	ctx := context.Background()
	target, err := z.Resolve(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	gate := newEchoGate(name, zellijClients(z, target), "/proc")
	t.Cleanup(gate.close)
	gateIs := func(what string, masked bool) {
		t.Helper()
		waitFor(t, what, 20*time.Second, func() bool {
			gate.refresh()
			return gate.masked(time.Now()) == masked
		})
	}
	write := func(chars string) {
		t.Helper()
		if out, err := exec.Command("zellij", "--session", name, "action", "write-chars", chars).CombinedOutput(); err != nil {
			t.Fatalf("write-chars: %v %s", err, out)
		}
	}
	gateIs("the shell prompt (echo on)", false)
	write("stty -echo; read x; stty echo\n")
	gateIs("the password read (echo off)", true)
	write("secret\n")
	gateIs("echo restored", false)

	// A second client through the proxy.
	term := newAttachTerm(t, 30, 100)
	sink := &keysSink{}
	binding := keysBinding{Recording: strings.Repeat("f", 32)}
	done := term.run(attachRun{argv: []string{"zellij", "attach", name}, session: name, policy: &gatePolicy{binding: binding, gate: gate}, send: sink.send})
	term.waitRaw()
	waitFor(t, "the proxied client", 20*time.Second, func() bool {
		out, err := exec.Command("zellij", "--session", name, "action", "list-clients").Output()
		return err == nil && strings.Count(strings.TrimSpace(string(out)), "\n") >= 2
	})
	term.typeKeys("e", "c", "h", "o", " ", "P", "R", "O", "X", "Y", "\r")
	term.waitOutput("PROXY")
	write("stty -echo; read y; stty echo\n")
	gateIs("the second password read", true)
	term.typeKeys("h", "u", "n", "t", "e", "r", "2", "\r")
	gateIs("echo restored after the second read", false)
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("zellij", "kill-session", name).Run()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("proxied client did not end with the session")
	}
	term.requireRestored()
	joined := strings.Join(sink.strokes(t), " ")
	prefix := binding.Recording + ":"
	if !strings.Contains(joined, prefix+"echo PROXY") || !strings.Contains(joined, prefix+"•") {
		t.Fatalf("strokes %s", joined)
	}
	for _, leak := range []string{"hunt", "unter", "er2"} {
		if strings.Contains(sink.raw(), leak) {
			t.Fatalf("password leaked (%q): %s", leak, joined)
		}
	}
}
