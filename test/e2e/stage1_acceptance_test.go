package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStage1JoinRejectsRetiredFlags(t *testing.T) {
	h := newHub(t)
	tests := []struct {
		name string
		args []string
	}{
		{name: "name", args: []string{"join", "--name", "mac"}},
		{name: "service", args: []string{"join", "--service"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := runCLI(t, h.binary, test.args...)
			requireFailure(t, output, err)
			if !strings.Contains(output, "flag provided but not defined: -"+test.name) {
				t.Fatalf("join %v did not reject retired flag as undefined: %s", test.args, output)
			}
		})
	}
}

func TestStage1RevokedNodeExits(t *testing.T) {
	h := newHub(t)
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	mustController(t, h, "rm", "mac")
	requireNonZeroProcessExit(t, n.process, statusTimeout)
	restarted := h.restartNode(t, n)
	requireNonZeroProcessExit(t, restarted.process, statusTimeout)
}

func TestStage1ExpiredNodeExits(t *testing.T) {
	h := newHub(t)
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	mustController(t, h, "ttl", "mac", "2s")
	requireNonZeroProcessExit(t, n.process, statusTimeout)
}

func TestStage1GracefulShutdown(t *testing.T) {
	h := newHub(t)
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	if err := terminateProcessTree(n.process.cmd); err != nil {
		t.Fatalf("signal node: %v", err)
	}
	requireCleanProcessExit(t, n.process, 5*time.Second)
	if err := terminateProcessTree(h.process.cmd); err != nil {
		t.Fatalf("signal hub: %v", err)
	}
	requireCleanProcessExit(t, h.process, 5*time.Second)
}

type apiRequest struct {
	method    string
	targetURL string
	token     string
	body      string
}

func TestStage1HTTPErrorClassification(t *testing.T) {
	h := newHub(t)
	joinArgs := h.invite(t, "mac")
	n := h.joinAgain(t, joinArgs)
	h.waitDevice(t, "mac", "alive")
	client := trustedHTTPClient(t, h.caPath)
	deviceToken := n.deviceToken(t)
	joinBody := usedJoinBody(t, joinArgs)
	requests := []struct {
		name     string
		call     apiRequest
		accepted []int
	}{
		{
			name: "ttl missing device",
			call: apiRequest{
				method: http.MethodPut, targetURL: h.url + "/v1/devices/missing/ttl",
				token: h.token, body: `{"ttl_seconds":60}`,
			},
			accepted: []int{http.StatusNotFound},
		},
		{
			name: "remove missing device",
			call: apiRequest{
				method: http.MethodDelete, targetURL: h.url + "/v1/devices/missing", token: h.token,
			},
			accepted: []int{http.StatusNotFound},
		},
		{
			name: "reuse invite",
			call: apiRequest{
				method: http.MethodPost, targetURL: h.url + "/v1/join", body: joinBody,
			},
			accepted: []int{http.StatusUnauthorized},
		},
		{
			name: "device token lists devices",
			call: apiRequest{
				method: http.MethodGet, targetURL: h.url + "/v1/devices", token: deviceToken,
			},
			accepted: []int{http.StatusUnauthorized, http.StatusForbidden},
		},
		{
			name: "device token creates invite",
			call: apiRequest{
				method: http.MethodPost, targetURL: h.url + "/v1/invites", token: deviceToken, body: `{}`,
			},
			accepted: []int{http.StatusUnauthorized, http.StatusForbidden},
		},
		{
			name: "invalid device token opens node connection",
			call: apiRequest{
				method: http.MethodGet, targetURL: h.url + "/v1/node", token: "revoked-or-invalid",
			},
			accepted: []int{http.StatusUnauthorized, http.StatusForbidden},
		},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			assertAPIStatus(t, client, request.call, request.accepted...)
		})
	}
}

func usedJoinBody(t *testing.T, joinArgs []string) string {
	t.Helper()
	code := ""
	for i := 1; i+1 < len(joinArgs); i += 2 {
		if joinArgs[i] == "--code" {
			code = joinArgs[i+1]
		}
	}
	if code == "" {
		t.Fatalf("join command has no code: %v", joinArgs)
	}
	payload, err := json.Marshal(struct {
		Code  string `json:"code"`
		OS    string `json:"os"`
		Arch  string `json:"arch"`
		Shell string `json:"shell"`
	}{Code: code, OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func assertAPIStatus(t *testing.T, client *http.Client, call apiRequest, accepted ...int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, call.method, call.targetURL, strings.NewReader(call.body))
	if err != nil {
		t.Fatal(err)
	}
	if call.token != "" {
		request.Header.Set("Authorization", "Bearer "+call.token)
	}
	if call.body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", call.method, call.targetURL, err)
	}
	_ = response.Body.Close()
	for _, status := range accepted {
		if response.StatusCode == status {
			return
		}
	}
	t.Fatalf("%s %s returned %s, want %v", call.method, call.targetURL, response.Status, accepted)
}
