package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testGitHubCfg = GitHubHookConfig{
	Secret:   "s3cret",
	Agent:    "reviewer",
	BotLogin: "wealthyautobot",
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubSignature(t *testing.T) {
	// Test vector from GitHub's "Validating webhook deliveries" docs.
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	good := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"

	if !verifyGitHubSignature(secret, body, good) {
		t.Fatal("documented signature rejected")
	}
	for name, tc := range map[string]struct {
		secret, body []byte
		header       string
	}{
		"tampered body":  {secret, []byte("Hello, World?"), good},
		"wrong secret":   {[]byte("nope"), body, good},
		"empty secret":   {nil, body, good},
		"missing header": {secret, body, ""},
		"sha1 header":    {secret, body, "sha1=0123"},
		"not hex":        {secret, body, "sha256=zz"},
	} {
		if verifyGitHubSignature(tc.secret, tc.body, tc.header) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func prEvent(action, extra string) string {
	return `{"action":"` + action + `","before":"aaa111",` + extra + `
		"pull_request":{"number":42,"state":"open","draft":false,"user":{"login":"dev"},
			"head":{"sha":"bbb222","repo":{"full_name":"dev/taxy"}}},
		"repository":{"full_name":"wealthy/taxy"},"sender":{"login":"dev"}}`
}

func TestParseGitHubReviewPullRequest(t *testing.T) {
	rv, reason, err := parseGitHubReview("pull_request", []byte(prEvent("opened", "")), testGitHubCfg)
	if err != nil || rv == nil {
		t.Fatalf("opened: rv=%v reason=%q err=%v", rv, reason, err)
	}
	if rv.Mode != "full" || rv.Repo != "wealthy/taxy" || rv.Number != 42 || rv.HeadSHA != "bbb222" || rv.HeadRepo != "dev/taxy" || rv.Force {
		t.Errorf("opened: %+v", rv)
	}

	rv, _, _ = parseGitHubReview("pull_request", []byte(prEvent("synchronize", "")), testGitHubCfg)
	if rv == nil || rv.Mode != "incremental" || rv.PreviousSHA != "aaa111" {
		t.Errorf("synchronize: %+v", rv)
	}

	rv, _, _ = parseGitHubReview("pull_request", []byte(prEvent("review_requested", `"requested_reviewer":{"login":"WealthyAutobot"},`)), testGitHubCfg)
	if rv == nil || rv.Mode != "full" || !rv.Force || rv.RequestedBy != "dev" {
		t.Errorf("review_requested from bot: %+v", rv)
	}

	ignored := map[string]string{
		"other reviewer": prEvent("review_requested", `"requested_reviewer":{"login":"alice"},`),
		"closed action":  prEvent("closed", ""),
		"draft":          strings.Replace(prEvent("opened", ""), `"draft":false`, `"draft":true`, 1),
		"not open":       strings.Replace(prEvent("opened", ""), `"state":"open"`, `"state":"closed"`, 1),
		"bot sender":     strings.Replace(prEvent("synchronize", ""), `"sender":{"login":"dev"}`, `"sender":{"login":"wealthyautobot"}`, 1),
	}
	for name, body := range ignored {
		rv, reason, err := parseGitHubReview("pull_request", []byte(body), testGitHubCfg)
		if err != nil || rv != nil || reason == "" {
			t.Errorf("%s: rv=%v reason=%q err=%v", name, rv, reason, err)
		}
	}
}

func commentEvent(body, author string, onPR bool) string {
	pr := ""
	if onPR {
		pr = `"pull_request":{"url":"x"},`
	}
	return `{"action":"created","issue":{"number":7,"state":"open",` + pr + `"user":{"login":"dev"}},
		"comment":{"body":"` + body + `","user":{"login":"` + author + `"}},
		"repository":{"full_name":"wealthy/taxy"}}`
}

func TestParseGitHubReviewComment(t *testing.T) {
	rv, _, err := parseGitHubReview("issue_comment", []byte(commentEvent(`/review`, "lead", true)), testGitHubCfg)
	if err != nil || rv == nil || rv.Number != 7 || rv.Trigger != "comment" || rv.RequestedBy != "lead" || !rv.Force {
		t.Fatalf("/review: rv=%+v err=%v", rv, err)
	}
	for name, body := range map[string]string{
		"on an issue":   commentEvent(`/review`, "lead", false),
		"by the bot":    commentEvent(`/review`, "wealthyautobot", true),
		"plain comment": commentEvent(`looks good, will /review later`, "lead", true),
	} {
		if rv, _, _ := parseGitHubReview("issue_comment", []byte(body), testGitHubCfg); rv != nil {
			t.Errorf("%s: reviewed", name)
		}
	}
}

func TestIsReviewCommand(t *testing.T) {
	yes := []string{"/review", "  /review please\nmore", "/Review", "@wealthyautobot review", "@WealthyAutobot Review again"}
	no := []string{"", "review", "/reviewed", "please /review", "@wealthyautobot", "@someone review", "lgtm\n/review"}
	for _, s := range yes {
		if !isReviewCommand(s, "wealthyautobot") {
			t.Errorf("%q should match", s)
		}
	}
	for _, s := range no {
		if isReviewCommand(s, "wealthyautobot") {
			t.Errorf("%q should not match", s)
		}
	}
}

func TestGitHubReviewMessageAndSession(t *testing.T) {
	rv := &githubReview{Repo: "wealthy/Taxy", Number: 5319, Mode: "incremental", Trigger: "synchronize", HeadSHA: "bbb", PreviousSHA: "aaa"}
	if got := rv.session(); got != "github:wealthy:taxy:pr-5319" {
		t.Errorf("session = %q", got)
	}
	msg := rv.message("wealthyautobot")
	for _, want := range []string{"repo: wealthy/Taxy", "pr: 5319", "mode: incremental", "previous_sha: aaa", "https://github.com/wealthy/Taxy/pull/5319", "as wealthyautobot", "untrusted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestSeenSet(t *testing.T) {
	s := newSeenSet(time.Hour)
	now := time.Now()
	if !s.add("k", now) || s.add("k", now.Add(time.Minute)) {
		t.Fatal("second add within ttl should report duplicate")
	}
	if !s.add("k", now.Add(2*time.Hour)) {
		t.Error("add after ttl should be new")
	}
	s.remove("k")
	if !s.add("k", now.Add(2*time.Hour)) {
		t.Error("add after remove should be new")
	}
}

func TestGitHubWebhookHandler(t *testing.T) {
	post := func(h *Handlers, event, body, sig string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", event)
		r.Header.Set("X-Hub-Signature-256", sig)
		w := httptest.NewRecorder()
		h.GitHubWebhook(w, r)
		return w
	}
	on := &Handlers{github: testGitHubCfg, githubSeen: newSeenSet(time.Hour)}
	off := &Handlers{githubSeen: newSeenSet(time.Hour)}

	if w := post(off, "ping", `{}`, sign("s3cret", `{}`)); w.Code != 404 {
		t.Errorf("disabled: %d", w.Code)
	}
	if w := post(on, "ping", `{}`, sign("wrong", `{}`)); w.Code != 401 {
		t.Errorf("bad signature: %d", w.Code)
	}
	if w := post(on, "ping", `{}`, sign("s3cret", `{}`)); w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Errorf("ping: %d %s", w.Code, w.Body)
	}
	draft := strings.Replace(prEvent("opened", ""), `"draft":false`, `"draft":true`, 1)
	if w := post(on, "pull_request", draft, sign("s3cret", draft)); w.Code != 200 || !strings.Contains(w.Body.String(), "draft") {
		t.Errorf("ignored draft: %d %s", w.Code, w.Body)
	}
	if w := post(on, "pull_request", `{not json`, sign("s3cret", `{not json`)); w.Code != 400 {
		t.Errorf("bad json: %d", w.Code)
	}
}
