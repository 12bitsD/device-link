package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestStage3RegressionColonLocalDotDotAndJSONSchema(t *testing.T) {
	env := s3NewEnv(t)
	s3CheckColonLocalAndDotDot(t, env)
	s3CheckReplayTamperProtected(t, env)
}

func s3CheckColonLocalAndDotDot(t *testing.T, env *s3Env) {
	t.Helper()
	srcRoot := t.TempDir()
	colonSrc := filepath.Join(srcRoot, "12:00.log")
	s3WriteFile(t, colonSrc, []byte("colon-ok"), 0o644)
	dst := filepath.Join(t.TempDir(), "colon-dst")
	sum := s3RequireCompletedCP(t, s3RunCP(t, env.h, colonSrc, "node-b:"+dst))
	s3AssertSummary(t, sum, s3TransferSummary{files: 1, skipped: 0, transferred: 8, total: 8})
	s3AssertFileContent(t, filepath.Join(dst, "12:00.log"), "colon-ok")

	dotDotSrc := filepath.Join(srcRoot, "sub") + "/../12:00.log"
	resDotSrc := s3RunCP(t, env.h, dotDotSrc, "node-b:"+dst)
	requireFailure(t, resDotSrc.combined(), resDotSrc.err)

	dotDotDst := dst + "/sub/.."
	resDotDst := s3RunCP(t, env.h, colonSrc, "node-b:"+dotDotDst)
	requireFailure(t, resDotDst.combined(), resDotDst.err)
}

func s3CheckReplayTamperProtected(t *testing.T, env *s3Env) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "one.txt")
	s3WriteFile(t, src, []byte("hello"), 0o644)
	dst := filepath.Join(t.TempDir(), "tamper-dst")
	sum := s3RequireCompletedCP(t, s3RunCP(t, env.h, src, "node-b:"+dst))

	httpClient := s3HTTPClient(t, env.h.caPath)
	defer httpClient.CloseIdleConnections()
	replayURL := fmt.Sprintf("%s/v1/transfers/%s/upload?files=0&skipped=99&transfer_bytes=0", env.h.url, sum.id)
	req, err := http.NewRequest(http.MethodPut, replayURL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.h.token)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay upload status = %d, want 409 or 400", resp.StatusCode)
	}
	after := s3WaitTransferState(t, env.h, sum.id, "completed")
	if after.files != 1 || after.skipped != 0 || after.transferred != 5 {
		t.Fatalf("completed transfer counts tampered: %+v", after)
	}
}

func TestStage3RegressionCancelJSONAndNodeDropFailsTransfer(t *testing.T) {
	h := newHub(t)
	proxy := s3NewThrottleProxy(t, strings.TrimPrefix(h.url, "https://"))
	nodeA := s3JoinViaProxy(t, h, proxy.url, "node-a")
	_ = s3JoinViaProxy(t, h, proxy.url, "node-b")

	srcFile := filepath.Join(t.TempDir(), "cancel384.bin")
	s3WriteFile(t, srcFile, s3DeterministicBytes(s3CancelBytes), 0o644)
	dst1 := filepath.Join(t.TempDir(), "reg-dst-1")
	id1 := s3ParseDetachedID(t, s3RunCP(t, h, "--detach", "node-a:"+srcFile, "node-b:"+dst1))
	_ = s3WaitTransferState(t, h, id1, "running")
	if cancelRes := s3RunTransfer(t, h, "cancel", id1); cancelRes.err != nil {
		t.Fatalf("cancel failed: %v", cancelRes.err)
	}
	jsonRes := s3RunTransfer(t, h, "status", "--json", id1)
	if jsonRes.err != nil {
		t.Fatalf("status --json failed: %v", jsonRes.err)
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonRes.stdout), &rawMap); err != nil {
		t.Fatalf("unmarshal status json: %v", err)
	}
	if _, hasErr := rawMap["error"]; hasErr {
		t.Fatalf("unexpected undocumented 'error' field in status --json: %s", jsonRes.stdout)
	}

	dst2 := filepath.Join(t.TempDir(), "reg-dst-2")
	id2 := s3ParseDetachedID(t, s3RunCP(t, h, "--detach", "node-a:"+srcFile, "node-b:"+dst2))
	_ = s3WaitTransferState(t, h, id2, "running")
	nodeA.process.stop()
	_ = s3WaitTransferState(t, h, id2, "failed")
}
