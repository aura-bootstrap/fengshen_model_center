package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBrowserE2E requires an already installed agent-browser and Chrome. It never
// installs tools, downloads weights, starts inference, or uses an existing center.
func TestBrowserE2E(t *testing.T) {
	if os.Getenv("FENGSHEN_CENTER_BROWSER_E2E") != "1" {
		t.Skip("explicit opt-in required: FENGSHEN_CENTER_BROWSER_E2E=1")
	}
	const work = "W:/QoderCN"
	const screenshots = "C:/Users/User/Documents/Claude/screenshots/fengshen-slicer"
	for _, dir := range []string{work, screenshots} {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("required existing artifact directory %s: %v", dir, err)
		}
	}
	// Keep fixtureManager's temporary project and registry outside the repository.
	t.Setenv("TMP", work)
	t.Setenv("TEMP", work)
	session := "fengshen-center-e2e-" + opaqueID()[:12]
	logPath := filepath.Join(work, session+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	t.Logf("session=%s evidence=%s", session, logPath)
	var logMu sync.Mutex
	record := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		if _, err := fmt.Fprintf(log, format+"\n", args...); err != nil {
			t.Errorf("write evidence: %v", err)
		}
	}
	cli := os.Getenv("FENGSHEN_CENTER_BROWSER_CLI")
	if cli == "" {
		cli = filepath.Join(os.Getenv("APPDATA"), "npm", "node_modules", "agent-browser", "bin", "agent-browser-win32-x64.exe")
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatalf("installed native agent-browser required (no installation attempted): %v", err)
	}
	token := opaqueID() // Only newHandler and the normal embedded page consume it.
	command := func(required bool, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		argv := append([]string{"--session", session, "--json"}, args...)
		cmd := exec.CommandContext(ctx, cli, argv...)
		cmd.Dir = work
		// Do not inherit persistence, remote providers, proxies, or another CDP session.
		for _, item := range os.Environ() {
			key := strings.ToUpper(strings.SplitN(item, "=", 2)[0])
			if strings.HasPrefix(key, "AGENT_BROWSER_") || key == "HTTP_PROXY" || key == "HTTPS_PROXY" || key == "ALL_PROXY" {
				continue
			}
			cmd.Env = append(cmd.Env, item)
		}
		cmd.Env = append(cmd.Env, "AGENT_BROWSER_ALLOWED_DOMAINS=127.0.0.1", "AGENT_BROWSER_DEFAULT_TIMEOUT=25000")
		// A newly spawned Windows daemon can inherit standard handles. Use files,
		// not os/exec copy pipes, so Run waits only for the CLI process itself.
		stdout, err := os.CreateTemp(work, session+"-stdout-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(stdout.Name())
		defer stdout.Close()
		stderr, err := os.CreateTemp(work, session+"-stderr-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(stderr.Name())
		defer stderr.Close()
		// Windows may retain the daemon-start command's two handles briefly after
		// close returns. Those uniquely named, token-free logs may remain in work;
		// do not kill unrelated processes or use sleeps to force their deletion.
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err = cmd.Run()
		exit := -1
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
		outBytes, readErr := os.ReadFile(stdout.Name())
		errBytes, stderrErr := os.ReadFile(stderr.Name())
		if readErr != nil || stderrErr != nil {
			t.Fatalf("read CLI output: %v %v", readErr, stderrErr)
		}
		out := strings.ReplaceAll(string(outBytes), token, "[REDACTED]")
		errText := strings.ReplaceAll(string(errBytes), token, "[REDACTED]")
		record("CLI %q exit=%d\nstdout=%s\nstderr=%s", args, exit, out, errText)
		var envelope struct {
			Success *bool `json:"success"`
		}
		decodeErr := json.Unmarshal(outBytes, &envelope)
		if err != nil || exit != 0 || decodeErr != nil || envelope.Success == nil || !*envelope.Success {
			if required {
				t.Fatalf("agent-browser %q exit=%d error=%v json=%v (see %s)", args, exit, err, decodeErr, logPath)
			} else {
				t.Errorf("cleanup agent-browser %q exit=%d error=%v", args, exit, err)
			}
		}
		return []byte(out)
	}
	run := func(args ...string) []byte { return command(true, args...) }
	// Load the installed guide before this test issues any browser commands.
	run("skills", "get", "core")
	defer func() { command(false, "close") }()

	m := fixtureManager(t)
	weight := filepath.Join(m.root, "models", "demucs", "955717e8-a.th")
	put(t, weight, "abc")
	before, err := os.Stat(weight)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		body, readErr := os.ReadFile(weight)
		after, statErr := os.Stat(weight)
		if readErr != nil || statErr != nil || string(body) != "abc" || !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("fixture weight content/mtime changed: read=%v stat=%v", readErr, statErr)
			return
		}
		record("WEIGHT unchanged content=abc bytes=3 mtime=%s", after.ModTime().UTC().Format(time.RFC3339Nano))
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	_, port, _ := net.SplitHostPort(host)
	if port == "18120" || port == "18125" {
		listener.Close()
		t.Fatal("OS selected a reserved center port; refusing to use it")
	}
	origin := "http://" + host
	record("SERVER %s actual fixtureManager + newHandler + embedded web", origin)
	t.Logf("isolated server=%s", origin)
	type requestEvidence struct {
		method, path string
		status       int
	}
	var requestMu sync.Mutex
	var requests []requestEvidence
	registrations := make(chan requestEvidence, 8)
	type operationEvidence struct {
		requestEvidence
		id string
	}
	operations := make(chan operationEvidence, 16)
	handler := newHandler(m, token, host, nil)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(peer).IsLoopback() {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		// Observe status only: no headers, token, query, homepage HTML, or bodies.
		observed := &browserE2EStatusWriter{ResponseWriter: w, status: 200}
		handler.ServeHTTP(observed, r)
		e := requestEvidence{r.Method, r.URL.Path, observed.status}
		requestMu.Lock()
		requests = append(requests, e)
		requestMu.Unlock()
		record("HTTP peer=%s host=%s method=%s path=%s status=%d", peer, r.Host, e.method, e.path, e.status)
		if (r.Method == "PUT" || r.Method == "DELETE") && strings.HasPrefix(r.URL.Path, "/api/registrations/") {
			registrations <- e
		}
		if r.Method == "POST" && (r.URL.Path == "/api/operations/scan" || r.URL.Path == "/api/operations/verify") && e.status == http.StatusAccepted {
			op := m.State().LastAttempt
			if op != nil {
				operations <- operationEvidence{e, op.ID}
			}
		}
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("own server shutdown: %v", err)
			server.Close()
		}
		select {
		case err := <-served:
			if err != http.ErrServerClosed {
				t.Errorf("own server exit: %v", err)
			}
		case <-ctx.Done():
			t.Error("own server did not exit")
		}
	}()
	assertJS := func(condition, name string) {
		t.Helper()
		run("eval", "(() => { if (!("+condition+")) throw new Error("+fmt.Sprintf("%q", name)+"); return "+fmt.Sprintf("%q", name+": PASS")+"; })()")
	}
	waitJS := func(condition string) { run("wait", "--fn", condition) }
	click := func(name string) { run("find", "role", "button", "click", "--name", name, "--exact") }
	shot := func(name string) {
		path := filepath.Join(screenshots, session+"-"+name+".png")
		run("screenshot", path)
		t.Logf("screenshot=%s", path)
	}
	entered := make(chan struct{})
	releaseScan := make(chan struct{})
	m.beforeScan = func(ctx context.Context) {
		close(entered)
		select {
		case <-ctx.Done():
		case <-releaseScan:
		}
	}
	waitEntered := func(ch <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-ch:
			record("HOOK %s entered (real operation blocked)", name)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s hook was not entered", name)
		}
	}
	terminal := func(kind, status string) *Operation {
		t.Helper()
		// Browser has awaited terminal UI. Channel + timeout bounds the WaitGroup
		// synchronization before removing either hook (no sleep-based polling).
		finished := make(chan struct{})
		go func() { m.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Fatal("manager worker did not finish after browser terminal notice")
		}
		op := m.State().LastAttempt
		if op == nil || op.Kind != kind || op.Status != status || op.FinishedAt == nil {
			t.Fatalf("expected %s/%s, got %+v", kind, status, op)
		}
		record("MANAGER kind=%s status=%s result=%s hashes=%+v", op.Kind, op.Status, op.ResultID, op.Hashes)
		return op
	}
	waitNotice := func(text string) {
		waitJS("document.getElementById('notice').textContent.includes(" + fmt.Sprintf("%q", text) + ") && !document.getElementById('scan').disabled")
	}

	run("set", "viewport", "1440", "1000")
	run("open", origin)
	// Activate browser console/page-error observation before exercising error paths.
	run("console", "--clear")
	run("errors", "--clear")
	waitJS("!document.getElementById('scan').disabled && document.querySelectorAll('#models button').length === 12")
	run("snapshot", "-i")
	click("刷新扫描")
	waitEntered(entered, "beforeScan")
	if op := m.State().ActiveOperation; op == nil || op.Status != "running" {
		t.Fatalf("scan must still be blocked/running: %+v", op)
	}
	waitJS("!document.getElementById('cancel').disabled && !document.getElementById('cancel').hidden")
	click("取消当前操作")
	waitNotice("受限扫描：已取消（未完成）")
	cancelledScan := terminal("scan", "cancelled")
	if inv, err := m.Inventory(cancelledScan.ResultID); err != nil || inv.Complete || m.State().LastCompleteInventory != "" {
		t.Fatalf("cancelled scan published complete evidence: complete=%v err=%v", inv.Complete, err)
	}
	shot("scan-cancelled")
	close(releaseScan)
	m.beforeScan = nil
	click("刷新扫描")
	waitNotice("受限扫描：已完成")
	scan := terminal("scan", "completed")
	inv, err := m.Inventory(scan.ResultID)
	if err != nil || !inv.Complete || inv.Stale || m.State().LastCompleteInventory != inv.ID {
		t.Fatalf("full scan evidence invalid: complete=%v stale=%v err=%v", inv.Complete, inv.Stale, err)
	}

	// Real Tab then Enter, not JS focus/click, must transfer focus into details.
	run("fill", "#search", "Demucs")
	waitJS("document.querySelectorAll('#models button').length === 2")
	run("focus", "#clear")
	run("press", "Tab")
	assertJS("document.activeElement.getAttribute('aria-label') === '查看 Demucs · htdemucs 详情'", "Tab reaches Demucs directory button")
	run("press", "Enter")
	waitJS("document.activeElement.id === 'detail-heading' && document.activeElement.textContent === 'Demucs · htdemucs'")
	record("KEYBOARD Tab/Enter opened Demucs; activeElement=detail-heading")
	waitJS("document.querySelectorAll('#files input[type=checkbox]').length === 1 && !document.querySelector('[data-verify]').disabled")
	shot("keyboard-detail")
	files, _, err := m.Files(inv.ID, "demucs", "", 200)
	if err != nil || len(files) != 1 || files[0].Bytes != "3" {
		t.Fatalf("expected one tiny Demucs fixture: count=%d err=%v", len(files), err)
	}
	readEntered := make(chan struct{})
	releaseRead := make(chan struct{})
	var once sync.Once
	m.beforeRead = func(ctx context.Context) {
		once.Do(func() { close(readEntered) })
		select {
		case <-ctx.Done():
		case <-releaseRead:
		}
	}
	click("选择当前页")
	assertJS("document.querySelector('#files input[type=checkbox]').checked", "current page selected by browser")
	click("计算所选 SHA-256")
	waitEntered(readEntered, "beforeRead")
	if op := m.State().ActiveOperation; op == nil || op.Kind != "verify" || op.Status != "running" {
		t.Fatalf("hash must still be blocked/running: %+v", op)
	}
	waitJS("!document.getElementById('cancel').disabled && !document.getElementById('cancel').hidden")
	click("取消当前操作")
	waitNotice("文件 SHA-256：已取消（未完成）")
	cancelledHash := terminal("verify", "cancelled")
	if len(cancelledHash.Hashes) != 1 || cancelledHash.Hashes[0].Status != "cancelled" || cancelledHash.Hashes[0].SHA256 != "" {
		t.Fatalf("cancelled hash published valid digest: %+v", cancelledHash.Hashes)
	}
	files, _, err = m.Files(inv.ID, "demucs", "", 200)
	if err != nil || len(files) != 1 || files[0].Hash != nil {
		t.Fatalf("cancelled file retains hash: %+v err=%v", files, err)
	}
	assertJS("!/[a-f0-9]{64}/i.test(document.getElementById('files').textContent)", "cancelled hash has no visible valid digest")
	shot("hash-cancelled")
	close(releaseRead)
	m.beforeRead = nil
	click("选择当前页")
	click("计算所选 SHA-256")
	waitNotice("文件 SHA-256：已完成")
	verified := terminal("verify", "completed")
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if len(verified.Hashes) != 1 || verified.Hashes[0].Status != "fingerprint_computed" || verified.Hashes[0].SHA256 != digest || verified.Hashes[0].Bytes != "3" {
		t.Fatalf("abc fingerprint invalid: %+v", verified.Hashes)
	}
	assertJS("document.getElementById('files').textContent.includes('"+digest+"')", "successful SHA256 visible in real browser")
	shot("hash-completed")

	registrationStatus := func(want int, method ...string) {
		t.Helper()
		wantMethod := "PUT"
		if len(method) != 0 {
			wantMethod = method[0]
		}
		select {
		case e := <-registrations:
			if e.status != want || e.method != wantMethod || e.path != "/api/registrations/demucs" {
				t.Fatalf("registration HTTP expected %s %d: %+v", wantMethod, want, e)
			}
			record("ASSERT real browser %s registration status=%d", wantMethod, want)
		case <-time.After(15 * time.Second):
			t.Fatal("browser registration request missing")
		}
	}
	invalid := filepath.Join(m.root, "models", "missing-e2e.th")
	run("fill", "#registration-path", invalid)
	click("保存登记并新扫描")
	registrationStatus(422)
	waitJS("!document.getElementById('error').hidden && document.getElementById('error').textContent.includes('path_invalid') && !document.querySelector('[data-write]').disabled")
	assertJS("document.getElementById('registration-path').value === "+fmt.Sprintf("%q", invalid), "422 preserves invalid registration draft")
	click("查看 Demucs · htdemucs_ft 详情")
	click("查看 Demucs · htdemucs 详情")
	assertJS("document.getElementById('registration-path').value === "+fmt.Sprintf("%q", invalid), "422 draft survives leaving and reopening details")
	if m.State().Registry.Revision != 0 || m.State().LastAttempt.ID != verified.ID {
		t.Fatal("422 changed revision or started an operation")
	}
	shot("registration-422-draft")

	valid := filepath.Dir(weight)
	run("fill", "#registration-path", valid)
	// Advance the actual manager's registry while the browser keeps revision 0.
	updated, err := m.Registration(context.Background(), "demucs", valid, 0, false)
	if err != nil || updated.Revision != 1 {
		t.Fatalf("prepare actual stale form: revision=%d err=%v", updated.Revision, err)
	}
	assertJS("document.getElementById('history').textContent.includes('登记版本 0')", "browser form still holds stale revision 0")
	click("保存登记并新扫描")
	registrationStatus(409)
	waitJS("!document.getElementById('error').hidden && document.getElementById('error').textContent.includes('registry_conflict') && document.getElementById('history').textContent.includes('登记版本 1') && !document.querySelector('[data-write]').disabled")
	assertJS("document.getElementById('registration-path').value === "+fmt.Sprintf("%q", valid), "409 preserves draft and reconciles actual revision")
	if state := m.State(); state.Registry.Revision != 1 || state.LastAttempt.ID != verified.ID || state.ActiveOperation != nil {
		t.Fatal("409 replayed write or started a scan")
	}
	shot("registration-409-conflict")

	// Add the pagination fixtures only now: all single-file cancellation assertions
	// above must continue to exercise the original abc weight alone.
	mtimes := map[string]time.Time{weight: before.ModTime()}
	for i := 0; i < 204; i++ {
		path := filepath.Join(valid, fmt.Sprintf("955717e8-e2e-%03d.th", i))
		put(t, path, "abc")
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		mtimes[path] = st.ModTime()
	}
	unchanged := func() {
		t.Helper()
		entries, err := os.ReadDir(valid)
		if err != nil || len(entries) != 205 {
			t.Errorf("fixture directory expected exactly 205 files: count=%d err=%v", len(entries), err)
		}
		for path, modified := range mtimes {
			body, readErr := os.ReadFile(path)
			st, statErr := os.Stat(path)
			if readErr != nil || statErr != nil || string(body) != "abc" || !st.ModTime().Equal(modified) {
				t.Errorf("fixture changed %s: read=%v stat=%v", path, readErr, statErr)
				continue
			}
			record("FILE unchanged path=%s bytes=3 mtime=%s", path, st.ModTime().UTC().Format(time.RFC3339Nano))
		}
		record("ASSERT original + 204 micro weights retain content/mtime; directory entries=%d", len(entries))
	}
	defer unchanged() // Preserve evidence even if a later browser assertion fails.

	// The status-only observer also associates each accepted POST with its actual
	// manager ID. Drain the four earlier operations, then require a fresh ID before
	// waiting on the manager; an old completed notice is never sufficient.
	seenOperations := map[string]bool{}
	for i := 0; i < 4; i++ {
		select {
		case e := <-operations:
			if e.id == "" || seenOperations[e.id] {
				t.Fatalf("missing or duplicate earlier operation ID: %+v", e)
			}
			seenOperations[e.id] = true
		default:
			t.Fatal("missing earlier accepted operation evidence")
		}
	}
	completedOperation := func(kind string) *Operation {
		t.Helper()
		var accepted operationEvidence
		select {
		case accepted = <-operations:
		case <-time.After(15 * time.Second):
			t.Fatal("new browser operation POST missing")
		}
		if accepted.id == "" || seenOperations[accepted.id] || accepted.path != "/api/operations/"+kind || accepted.status != 202 {
			t.Fatalf("expected fresh accepted %s operation: %+v", kind, accepted)
		}
		seenOperations[accepted.id] = true
		op := terminal(kind, "completed")
		if op.ID != accepted.id || m.State().ActiveOperation != nil {
			t.Fatalf("completed operation identity mismatch: accepted=%s manager=%+v", accepted.id, op)
		}
		waitJS("performance.getEntriesByType('resource').some(e => new URL(e.name).pathname === '/api/operations/" + op.ID + "') && document.getElementById('operation-text').textContent.includes('已完成') && !document.getElementById('scan').disabled")
		record("ASSERT fresh browser operation id=%s kind=%s completed, manager worker joined", op.ID, kind)
		return op
	}
	completeInventory := func(op *Operation, revision uint64, previous string) Inventory {
		t.Helper()
		state := m.State()
		inventory, err := m.Inventory(op.ResultID)
		if err != nil || inventory.ID == "" || inventory.ID == previous || !inventory.Complete || inventory.Stale || inventory.RegistryRevision != revision || state.Registry.Revision != revision || state.CurrentInventory != inventory.ID || state.LastCompleteInventory != inventory.ID {
			t.Fatalf("new complete inventory/revision invalid: inventory=%+v state=%+v err=%v", inventory, state, err)
		}
		found := false
		for _, model := range inventory.Models {
			if model.ID == "demucs" {
				found = model.FileCount == 205 && model.DiscoveryStatus == "found" && model.IdentityStatus == "recognized_by_rule" && model.Size.Bytes != nil && *model.Size.Bytes == "615" && model.Size.CountedFiles == 205 && model.Inference == "not_tested"
			}
		}
		if !found {
			t.Fatal("new inventory must identify 205 distinct Demucs file objects / 615 bytes without inference")
		}
		waitNotice("受限扫描：已完成")
		waitJS("document.getElementById('inventory-meta').textContent.includes('" + inventory.ID + "') && document.getElementById('history').textContent.includes('登记版本 " + fmt.Sprint(revision) + "') && document.getElementById('error').hidden && document.querySelectorAll('#files input[type=checkbox]').length === 200 && !document.querySelector('[data-verify]').disabled")
		record("INVENTORY id=%s operation=%s revision=%d demucs=205 bytes=615 inference=not_tested", inventory.ID, op.ID, revision)
		return inventory
	}
	assertPage := func(page []ModelFile, number int) {
		t.Helper()
		labels := make([]string, 0, len(page))
		for _, file := range page {
			if file.ID == "" || file.Bytes != "3" || file.Evidence != "recognized_by_rule" {
				t.Fatalf("unexpected Demucs page record: %+v", file)
			}
			if _, ok := mtimes[file.Path]; !ok {
				t.Fatalf("page refers to an unexpected fixture path: %s", file.Path)
			}
			labels = append(labels, "选择文件 "+file.Path)
			record("PAGE page=%d file_id=%s path=%s bytes=%s", number, file.ID, file.Path, file.Bytes)
		}
		encoded, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		waitJS(fmt.Sprintf("document.querySelectorAll('#files input[type=checkbox]').length === %d && document.getElementById('files').textContent.includes('第 %d 页') && !document.querySelector('[data-verify]').disabled", len(page), number))
		assertJS("JSON.stringify(Array.from(document.querySelectorAll('#files article.file > label span'), e => e.textContent)) === JSON.stringify("+string(encoded)+")", fmt.Sprintf("page %d exactly matches actual manager records (%d files)", number, len(page)))
		assertJS(fmt.Sprintf("Array.from(document.querySelectorAll('#files button')).find(b => b.textContent === '上一页').disabled === %t && Array.from(document.querySelectorAll('#files button')).find(b => b.textContent === '下一页').disabled === %t", number == 1, number == 2), fmt.Sprintf("page %d navigation boundary", number))
	}

	// Reuse the reconciled revision-1 form, not a synthetic fetch or DOM rewrite.
	assertJS("document.getElementById('registration-path').value === "+fmt.Sprintf("%q", valid)+" && document.getElementById('history').textContent.includes('登记版本 1')", "legal directory draft ready at revision 1")
	click("保存登记并新扫描")
	registrationStatus(200)
	savedScan := completedOperation("scan")
	savedInventory := completeInventory(savedScan, 2, inv.ID)
	registered, ok := m.State().Registry.Registrations["demucs"]
	if !ok || len(m.State().Registry.Registrations) != 1 || registered.InputPath != valid || registered.ResolvedPath != valid || registered.Kind != "directory" || registered.Identity == "" {
		t.Fatalf("revision 2 directory registration identity invalid: %+v", registered)
	}
	record("REGISTRY revision=2 model=demucs kind=%s identity=%s path=%s", registered.Kind, registered.Identity, registered.ResolvedPath)
	firstPage, cursor, err := m.Files(savedInventory.ID, "demucs", "", 200)
	if err != nil || len(firstPage) != 200 || cursor == "" {
		t.Fatalf("first page expected 200 + opaque next cursor: count=%d err=%v", len(firstPage), err)
	}
	assertPage(firstPage, 1)
	shot("registration-200-scan-completed-page-1")
	click("下一页")
	lastPage, next, err := m.Files(savedInventory.ID, "demucs", cursor, 200)
	if err != nil || len(lastPage) != 5 || next != "" {
		t.Fatalf("last page expected 5 and no next cursor: count=%d next=%q err=%v", len(lastPage), next, err)
	}
	ids := map[string]bool{}
	for _, file := range firstPage {
		ids[file.ID] = true
	}
	for _, file := range lastPage {
		if ids[file.ID] {
			t.Fatalf("pagination repeats file ID %s", file.ID)
		}
		ids[file.ID] = true
	}
	if len(ids) != 205 {
		t.Fatalf("expected 205 unique paginated IDs, got %d", len(ids))
	}
	assertPage(lastPage, 2)
	click("选择当前页")
	assertJS("document.querySelectorAll('#files input[type=checkbox]:checked').length === 5 && document.getElementById('files').textContent.includes('第 2 页 · 已选 5 项')", "browser selects exactly five last-page records")
	click("计算所选 SHA-256")
	pageHash := completedOperation("verify")
	if pageHash.ID == savedScan.ID || pageHash.ResultID != "" || len(pageHash.Hashes) != 5 {
		t.Fatalf("last-page verify operation invalid: %+v", pageHash)
	}
	wantIDs := map[string]bool{}
	for _, file := range lastPage {
		wantIDs[file.ID] = true
	}
	for _, hash := range pageHash.Hashes {
		if !wantIDs[hash.FileID] || hash.Status != "fingerprint_computed" || hash.SHA256 != digest || hash.Bytes != "3" || hash.CheckedAt.IsZero() {
			t.Fatalf("last-page actual ID / known SHA mismatch: %+v", hash)
		}
		delete(wantIDs, hash.FileID)
	}
	if len(wantIDs) != 0 {
		t.Fatal("verification omitted selected last-page IDs")
	}
	lastPage, next, err = m.Files(savedInventory.ID, "demucs", cursor, 200)
	if err != nil || len(lastPage) != 5 || next != "" || m.State().Registry.Revision != 2 || m.State().CurrentInventory != savedInventory.ID {
		t.Fatalf("hash changed inventory/revision or pagination: count=%d err=%v", len(lastPage), err)
	}
	for _, file := range lastPage {
		if file.Hash == nil || file.Hash.FileID != file.ID || file.Hash.Status != "fingerprint_computed" || file.Hash.SHA256 != digest {
			t.Fatalf("hash not attached to actual last-page file: %+v", file)
		}
	}
	firstPage, _, err = m.Files(savedInventory.ID, "demucs", "", 200)
	if err != nil || len(firstPage) != 200 {
		t.Fatalf("first page after last-page verify: %v", err)
	}
	for _, file := range firstPage {
		if file.Hash != nil {
			t.Fatalf("last-page verification leaked onto unselected first-page ID %s", file.ID)
		}
	}
	waitNotice("文件 SHA-256：已完成")
	assertPage(lastPage, 2)
	waitJS("document.querySelectorAll('#files article.file').length === 5 && Array.from(document.querySelectorAll('#files article.file')).every(e => e.textContent.includes('" + digest + "') && e.textContent.includes('已计算指纹（非认证）'))")
	shot("page-2-five-known-sha-completed")
	click("上一页")
	assertPage(firstPage, 1)
	click("下一页")
	assertPage(lastPage, 2)
	waitJS("Array.from(document.querySelectorAll('#files article.file')).every(e => e.textContent.includes('" + digest + "'))")

	// Exercise the real narrow-layout action region before removing registration.
	// Native focus/Tab scrolls into view; eval only reads geometry and hit targets.
	assertReachable := func(selector, name string) {
		t.Helper()
		assertJS("(() => { const e = document.querySelector("+fmt.Sprintf("%q", selector)+"); if (!e || e.disabled || document.activeElement !== e) return false; const r = e.getBoundingClientRect(); const hit = document.elementFromPoint(r.x + r.width/2, r.y + r.height/2); return scrollY > 0 && r.width > 0 && r.height > 0 && r.left >= 0 && r.right <= innerWidth && r.top >= 0 && r.bottom <= innerHeight && (hit === e || e.contains(hit)); })()", name)
	}
	run("set", "viewport", "360", "1000")
	waitJS("innerWidth === 360")
	run("scroll", "up", "100000")
	run("scrollintoview", "#registration-path")
	run("focus", "#registration-path")
	run("press", "Tab")
	assertReachable("#detail-body form button[data-write]", "360px scrolled save button keyboard and pointer reachable")
	run("press", "Tab")
	assertJS("document.activeElement.textContent === '取消中心登记并新扫描'", "Tab reaches actual registered-directory remove action")
	assertReachable("#detail-body section > button[data-write]", "360px scrolled remove button keyboard and pointer reachable")
	run("snapshot", "-i", "-s", "#detail-body")
	shot("360-detail-registration-actions")
	run("scrollintoview", "#files .actions button:first-child")
	run("focus", "#files .actions button:first-child")
	run("press", "Tab")
	run("press", "Tab")
	assertReachable("#files [data-verify]", "360px scrolled SHA action keyboard and pointer reachable")
	assertJS("document.documentElement.scrollWidth <= innerWidth && document.body.scrollWidth <= innerWidth", "360px detail with known SHA has no horizontal overflow")
	shot("360-detail-sha-actions")
	run("set", "viewport", "1440", "1000")

	click("取消中心登记并新扫描")
	registrationStatus(200, "DELETE")
	removedScan := completedOperation("scan")
	removedInventory := completeInventory(removedScan, 3, savedInventory.ID)
	if len(m.State().Registry.Registrations) != 0 {
		t.Fatal("revision 3 DELETE must remove only the center registration")
	}
	assertJS("document.getElementById('registration-path').value === '' && !Array.from(document.querySelectorAll('#detail-body button')).some(b => b.textContent === '取消中心登记并新扫描') && document.getElementById('detail-body').textContent.includes('未登记：使用默认受限发现规则。')", "DELETE removes registration and restores default discovery in browser")
	record("REGISTRY revision=3 registrations=0 operation=%s inventory=%s; default discovery still finds 205 weights", removedScan.ID, removedInventory.ID)
	unchanged()
	shot("registration-delete-200-scan-completed")

	run("select", "#category", "音色处理")
	run("select", "#runtime", "Go")
	waitJS("document.querySelectorAll('#models button').length === 0 && !document.getElementById('empty').hidden && document.getElementById('model-count').textContent === '0 / 12'")
	assertJS("document.getElementById('category').value === '音色处理' && document.getElementById('runtime').value === 'Go'", "incompatible actual browser filters produce empty state")
	shot("filters-empty")
	click("清除搜索与筛选")
	waitJS("document.querySelectorAll('#models button').length === 12 && document.getElementById('empty').hidden && document.getElementById('model-count').textContent === '12 / 12' && document.activeElement.id === 'search'")
	assertJS("['search','category','runtime','status'].every(id => document.getElementById(id).value === '') && document.getElementById('detail-heading').textContent === 'Demucs · htdemucs' && document.querySelector('#models button[aria-pressed=true]').getAttribute('aria-label') === '查看 Demucs · htdemucs 详情'", "clear restores all twelve entries without changing selected model identity")
	shot("filters-clear-restored")
	// Existing web/app.test.cjs covers HTML-shaped path, evidence, diagnostic and
	// hash message injection as text. Do not create Windows-illegal <> filenames
	// or mutate the browser DOM to manufacture a passing injection assertion.
	record("INJECTION delegated to existing web unit text-only path/evidence/diagnostic coverage; all 205 real filenames are Windows-legal")

	for _, width := range []string{"1440", "360"} {
		run("set", "viewport", width, "1000")
		waitJS("window.innerWidth === " + width)
		assertJS("document.documentElement.scrollWidth <= window.innerWidth && document.body.scrollWidth <= window.innerWidth", width+"px has no horizontal overflow")
		run("eval", "JSON.stringify({viewport:innerWidth,documentWidth:document.documentElement.scrollWidth,bodyWidth:document.body.scrollWidth})")
		run("scroll", "up", "100000")
		shot("viewport-" + width)
	}
	// Resource Timing exposes URLs only; never dump a HAR, headers or homepage body.
	assertJS("performance.getEntriesByType('resource').length > 0 && performance.getEntriesByType('resource').every(e => new URL(e.name).origin === location.origin && new URL(e.name).hostname === '127.0.0.1')", "all browser resource requests are own loopback origin")
	run("eval", "JSON.stringify(performance.getEntriesByType('resource').map(e => ({origin:new URL(e.name).origin,path:new URL(e.name).pathname,type:e.initiatorType})))")
	errors := run("errors")
	var pageErrors struct {
		Data struct {
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(errors, &pageErrors); err != nil || pageErrors.Data.Errors == nil || len(pageErrors.Data.Errors) != 0 {
		t.Fatalf("unexpected browser page errors: %s parse=%v", errors, err)
	}
	console := run("console")
	var consoleResult struct {
		Data struct {
			Messages []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(console, &consoleResult); err != nil || consoleResult.Data.Messages == nil {
		t.Fatalf("unrecognized console evidence: %s parse=%v", console, err)
	}
	for _, message := range consoleResult.Data.Messages {
		if message.Type == "error" && !(strings.Contains(message.Text, "Failed to load resource") && (strings.Contains(message.Text, "422") || strings.Contains(message.Text, "409") || strings.Contains(message.Text, "404"))) {
			t.Errorf("unexpected console error: %s", message.Text)
		}
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	puts, deletes, cancels, scans, verifies := 0, 0, 0, 0, 0
	for _, e := range requests {
		if e.method == "PUT" {
			puts++
		}
		if e.method == "DELETE" {
			deletes++
		}
		if e.method == "POST" && e.path == "/api/operations/scan" {
			scans++
		}
		if e.method == "POST" && e.path == "/api/operations/verify" {
			verifies++
		}
		if e.method == "POST" && strings.HasSuffix(e.path, "/cancel") {
			cancels++
		}
		if e.status >= 400 && !(e.path == "/api/registrations/demucs" && (e.status == 422 || e.status == 409)) && !(e.path == "/favicon.ico" && e.status == 404) {
			t.Errorf("unexpected HTTP failure: %+v", e)
		}
	}
	if puts != 3 || deletes != 1 || cancels != 2 || scans != 4 || verifies != 3 || len(seenOperations) != 7 || len(operations) != 0 || len(registrations) != 0 {
		t.Errorf("unexpected browser request counts PUT=%d DELETE=%d cancel=%d scan=%d verify=%d uniqueOperations=%d", puts, deletes, cancels, scans, verifies, len(seenOperations))
	}
	if state := m.State(); state.Registry.Revision != 3 || state.LastAttempt == nil || state.LastAttempt.ID != removedScan.ID || state.ActiveOperation != nil || state.CurrentInventory != removedInventory.ID {
		t.Error("read-only filters/viewports unexpectedly changed registry or operation identity")
	}
	record("SUMMARY browser PUTs=%d DELETEs=%d cancels=%d scans=%d verifies=%d uniqueOperations=%d revision=3 pages=200+5 unchangedFiles=205 pageErrors=0 consoleMessages=%d; only expected HTTP errors allowed; no replay, download or inference", puts, deletes, cancels, scans, verifies, len(seenOperations), len(consoleResult.Data.Messages))
}

// browserE2EStatusWriter forwards the real response without retaining its body.
type browserE2EStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *browserE2EStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
