package main

// Git smart HTTP: info/refs, git-upload-pack (protocol v2 and v0) and
// git-receive-pack (v0). The protocol follows git's wire exactly, because
// the suite speaks it with a real client: pkt-lines, sideband framing,
// v2 capability advertisement, ls-refs, shallow/deepen fetches and
// report-status pushes.
//
// Auth: every git request must carry the process's bearer token; a missing
// or wrong token is 401. A repository under another owner is 404, never 403.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "github-core-clone-forge/1.0.0"

const (
	uploadAdvertisement  = "application/x-git-upload-pack-advertisement"
	receiveAdvertisement = "application/x-git-receive-pack-advertisement"
	uploadResult         = "application/x-git-upload-pack-result"
	receiveResult        = "application/x-git-receive-pack-result"
)

// fetchKey is the cache key for a fetch response. The response depends
// only on the protocol version, the repo's objects and (wants, haves,
// depth, shallow); any new object clears the cache, so a cached response
// is never stale. This matters for the clone workload: 50 concurrent
// shallow clones of the same repo would otherwise each re-walk the
// 200-commit history and re-deflate the same pack.
func fetchKey(version string, repo *Repo, wants, haves []string, depth *int, shallow []string) string {
	sortedCopy := func(in []string) []string {
		out := make([]string, len(in))
		copy(out, in)
		sort.Strings(out)
		return out
	}
	depthText := ""
	if depth != nil {
		depthText = strconv.Itoa(*depth)
	}
	return version + "|" + repo.Owner + "/" + repo.Name + "|" +
		strings.Join(sortedCopy(wants), ",") + "|" +
		strings.Join(sortedCopy(haves), ",") + "|" +
		depthText + "|" +
		strings.Join(sortedCopy(shallow), ",")
}

// loadFetchResponse returns a cached fetch response, if one exists.
func loadFetchResponse(store *Store, key string) ([]byte, bool) {
	v, ok := store.packCache.Load(key)
	if !ok {
		return nil, false
	}
	if data, ok := v.([]byte); ok {
		return data, true
	}
	return nil, false
}

// storeFetchResponse stores a fetch response in the cache, evicting
// everything when full.
func storeFetchResponse(store *Store, key string, response []byte) []byte {
	count := 0
	store.packCache.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count >= 128 {
		store.packCache.Clear()
	}
	store.packCache.Store(key, response)
	return response
}

// gitRepo returns the repository a git path names, or nil (404: missing,
// or another owner).
func gitRepo(ctx *Ctx, owner, repoName string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return ctx.Store.GetRepo(owner, repoName)
}

// sortedRefs returns all refs of a repo, sorted by name, as (oid, name) pairs.
type refPair struct {
	oid  string
	name string
}

func sortedRefs(repo *Repo) []refPair {
	out := make([]refPair, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, refPair{oid: oid, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// peelOf returns the peeled commit of a ref that points at a tag, or "".
func peelOf(store *Store, repo *Repo, oid string) (string, bool) {
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != ObjTag {
		return "", false
	}
	return store.PeelToCommit(repo, oid)
}

// v0UploadCaps is the v0 capability list for upload-pack (the client
// requires side-band-64k and shallow).
func v0UploadCaps(repo *Repo) string {
	symref := "symref=HEAD:refs/heads/" + repo.DefaultBranch
	return symref + " side-band-64k thin-pack ofs-delta shallow multi_ack agent=" + gitAgent
}

// v0ReceiveCaps is the v0 capability list for receive-pack.
func v0ReceiveCaps() string {
	return "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent
}

// v0Advertisement builds the v0 advertisement body for a service.
func v0Advertisement(store *Store, repo *Repo, service string) []byte {
	var out []byte
	out = append(out, EncodePktLine([]byte("# service="+service+"\n"))...)
	// The smart-HTTP advertisement puts a flush after the service line,
	// before the refs (a real client requires it on receive-pack).
	out = append(out, FlushPkt...)
	refs := sortedRefs(repo)
	var caps string
	if service == "git-upload-pack" {
		caps = v0UploadCaps(repo)
	} else {
		caps = v0ReceiveCaps()
	}
	head, _ := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if len(refs) == 0 {
		// An unborn repo advertises only the capabilities pseudo-ref.
		out = append(out, EncodePktLine([]byte(ZeroOID+" capabilities^{}\x00"+caps+"\n"))...)
	} else {
		if head == "" {
			head = ZeroOID
		}
		out = append(out, EncodePktLine([]byte(head+" HEAD\x00"+caps+"\n"))...)
		for _, ref := range refs {
			out = append(out, EncodePktLine([]byte(ref.oid+" "+ref.name+"\n"))...)
			if peeled, ok := peelOf(store, repo, ref.oid); ok {
				out = append(out, EncodePktLine([]byte(peeled+" "+ref.name+"^{}\n"))...)
			}
		}
	}
	out = append(out, FlushPkt...)
	return out
}

// v2Advertisement is the v2 capability advertisement: it names no
// repository, so the same bytes answer every v2 info/refs request.
// Built once at startup instead of re-encoding the same pkt-lines per
// request.
var v2Advertisement = buildV2Advertisement()

func buildV2Advertisement() []byte {
	var out []byte
	out = append(out, EncodePktLine([]byte("# service=git-upload-pack\n"))...)
	out = append(out, FlushPkt...)
	for _, line := range []string{
		"version 2\n",
		"agent=" + gitAgent + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow\n",
		"object-format=sha1\n",
		"side-band-64k\n",
	} {
		out = append(out, EncodePktLine([]byte(line))...)
	}
	out = append(out, FlushPkt...)
	return out
}

// HandleInfoRefs handles GET /{owner}/{repo}.git/info/refs?service=<service>.
// A v2 request (Git-Protocol: version=2 on upload-pack) gets the v2
// advertisement; anything else gets the v0 advertisement with its service
// preamble.
func HandleInfoRefs(ctx *Ctx, w http.ResponseWriter, owner, repoName string) {
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return
	}
	repo := gitRepo(ctx, owner, repoName)
	if repo == nil {
		SendNotFound(w, "Not Found")
		return
	}
	service := ctx.Query.Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		SendError(w, http.StatusBadRequest, "unknown service")
		return
	}
	isV2 := service == "git-upload-pack" && ctx.Headers.Get("Git-Protocol") == "version=2"
	var body []byte
	if isV2 {
		body = v2Advertisement
	} else {
		body = v0Advertisement(ctx.Store, repo, service)
	}
	contentType := uploadAdvertisement
	if service == "git-receive-pack" {
		contentType = receiveAdvertisement
	}
	SendBytes(w, http.StatusOK, body, contentType, nil)
}

// lsRefLine renders one ls-refs answer line, with its attributes.
func lsRefLine(store *Store, repo *Repo, oid, name string, peel, symrefs bool) string {
	line := oid + " " + name
	if name == "HEAD" && symrefs {
		line += " symref-target:refs/heads/" + repo.DefaultBranch
	}
	if peel {
		if peeled, ok := peelOf(store, repo, oid); ok {
			line += " peeled:" + peeled
		}
	}
	return line + "\n"
}

// handleLsRefs handles a v2 ls-refs command.
func handleLsRefs(ctx *Ctx, repo *Repo, args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var lines [][]byte
	head, headOK := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if headOK {
		if matches("HEAD") {
			lines = append(lines, EncodePktLine([]byte(lsRefLine(ctx.Store, repo, head, "HEAD", peel, symrefs))))
		}
	} else if unborn {
		if matches("HEAD") {
			lines = append(lines, EncodePktLine([]byte("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n")))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !matches(ref.name) {
			continue
		}
		lines = append(lines, EncodePktLine([]byte(lsRefLine(ctx.Store, repo, ref.oid, ref.name, peel, symrefs))))
	}
	lines = append(lines, FlushPkt)
	return bytes.Join(lines, nil)
}

// sidebandPackets chunks pack bytes into sideband band-1 packets. A
// pkt-line is at most 65520 bytes total (4 for its length, 1 for the
// band), so a data chunk is at most 65515 bytes; one byte more and the
// packet is rejected. Each packet is built in one allocation: header,
// band byte and chunk copied into a single buffer.
func sidebandPackets(data []byte) [][]byte {
	const maxChunk = 65515
	var out [][]byte
	for at := 0; at < len(data); at += maxChunk {
		end := at + maxChunk
		if end > len(data) {
			end = len(data)
		}
		total := end - at + 1 + 4
		pkt := make([]byte, total)
		// Four lowercase hex digits, no fmt.Sprintf allocation on the
		// hot pack path: total is at most 65520, so all four nibbles
		// are always present.
		const hexDigits = "0123456789abcdef"
		pkt[0] = hexDigits[(total>>12)&0xf]
		pkt[1] = hexDigits[(total>>8)&0xf]
		pkt[2] = hexDigits[(total>>4)&0xf]
		pkt[3] = hexDigits[total&0xf]
		pkt[4] = 1
		copy(pkt[5:], data[at:end])
		out = append(out, pkt)
	}
	return out
}

var v2CommandRe = regexp.MustCompile(`^command=([a-z-]+)$`)

// parseV2Request parses a v2 command request: the command line, then
// argument lines.
func parseV2Request(body []byte) (string, []string, error) {
	packets := DecodePktLines(body)
	var lines []string
	for _, pkt := range packets {
		if pkt.Kind == PktData {
			lines = append(lines, strings.TrimSuffix(string(pkt.Data), "\n"))
		}
	}
	commandLine := ""
	if len(lines) > 0 {
		commandLine = lines[0]
	}
	m := v2CommandRe.FindStringSubmatch(commandLine)
	if m == nil {
		return "", nil, fmt.Errorf("bad v2 command line %s", JSONText(commandLine))
	}
	return m[1], lines[1:], nil
}

// lookupOf returns a lookup closure over a repo's objects.
func lookupOf(repo *Repo) func(oid string) *GitObject {
	return func(oid string) *GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			objCopy := obj
			return &objCopy
		}
		return nil
	}
}

// handleFetchV2 handles a v2 fetch command.
func handleFetchV2(ctx *Ctx, repo *Repo, args []string) []byte {
	var wants, haves, shallow []string
	var depth *int
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimSpace(strings.TrimPrefix(arg, "want ")))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(strings.TrimPrefix(arg, "have ")))
		case strings.HasPrefix(arg, "shallow "):
			shallow = append(shallow, strings.TrimSpace(strings.TrimPrefix(arg, "shallow ")))
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(arg, "deepen ")), 64); err == nil && n > 0 {
				d := int(n)
				depth = &d
			}
			// thin-pack, no-progress, ofs-delta, filter, agent,
			// object-format, deepen-since, deepen-not and done need no
			// action here.
		}
	}
	// A cached response is byte-identical: the response depends only on
	// the fetch parameters and the repo's objects, and any new object
	// clears it.
	key := fetchKey("v2", repo, wants, haves, depth, shallow)
	if cached, ok := loadFetchResponse(ctx.Store, key); ok {
		return cached
	}
	plan := PlanFetch(lookupOf(repo), wants, haves, depth, shallow)
	var parts [][]byte
	// Acknowledgments: only when the client sent haves. A fetch with no
	// haves (a clone) expects the packfile (or shallow-info) section
	// first; sending an acknowledgments section there is a protocol
	// error for a real client.
	if len(haves) > 0 {
		parts = append(parts, EncodePktLine([]byte("acknowledgments\n")))
		var acked []string
		for _, have := range haves {
			if _, ok := repo.Objects[have]; ok {
				acked = append(acked, have)
			}
		}
		if len(acked) == 0 {
			parts = append(parts, EncodePktLine([]byte("NAK\n")))
		} else {
			for _, have := range acked {
				parts = append(parts, EncodePktLine([]byte("ACK "+have+"\n")))
			}
			parts = append(parts, EncodePktLine([]byte("ready\n")))
		}
		parts = append(parts, FlushPkt)
	}
	// Shallow-info, when the boundary moved.
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		parts = append(parts, EncodePktLine([]byte("shallow-info\n")))
		for _, oid := range plan.Shallow {
			parts = append(parts, EncodePktLine([]byte("shallow "+oid+"\n")))
		}
		for _, oid := range plan.Unshallow {
			parts = append(parts, EncodePktLine([]byte("unshallow "+oid+"\n")))
		}
		parts = append(parts, FlushPkt)
	}
	// Packfile, only when there is something to send: an empty packfile
	// section is a protocol error for the client.
	if len(plan.Objects) > 0 {
		inputs := make([]PackObjectInput, len(plan.Objects))
		for i, obj := range plan.Objects {
			inputs[i] = PackObjectInput{Type: obj.Type, Data: obj.Data}
		}
		pack := WritePack(inputs)
		parts = append(parts, EncodePktLine([]byte("packfile\n")))
		parts = append(parts, sidebandPackets(pack)...)
		parts = append(parts, FlushPkt)
	}
	return storeFetchResponse(ctx.Store, key, bytes.Join(parts, nil))
}

// handleFetchV0 handles a v0 fetch: wants, then haves, in two
// flush-separated sections.
func handleFetchV0(ctx *Ctx, repo *Repo, body []byte) []byte {
	packets := DecodePktLines(body)
	var wants, haves, shallow []string
	var depth *int
	for _, pkt := range packets {
		if pkt.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(pkt.Data), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			// The first want line may carry space-separated features
			// after the oid.
			wants = append(wants, strings.Split(strings.TrimPrefix(line, "want "), " ")[0])
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(strings.TrimPrefix(line, "shallow ")))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "deepen ")), 64); err == nil && n > 0 {
				d := int(n)
				depth = &d
			}
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(strings.TrimPrefix(line, "have ")))
			// 'done' ends the negotiation.
		}
	}
	key := fetchKey("v0", repo, wants, haves, depth, shallow)
	if cached, ok := loadFetchResponse(ctx.Store, key); ok {
		return cached
	}
	plan := PlanFetch(lookupOf(repo), wants, haves, depth, shallow)
	var parts [][]byte
	var acked []string
	for _, have := range haves {
		if _, ok := repo.Objects[have]; ok {
			acked = append(acked, have)
		}
	}
	if len(acked) == 0 {
		parts = append(parts, EncodePktLine([]byte("NAK\n")))
	} else {
		for _, have := range acked {
			parts = append(parts, EncodePktLine([]byte("ACK "+have+"\n")))
		}
	}
	for _, oid := range plan.Shallow {
		parts = append(parts, EncodePktLine([]byte("shallow "+oid+"\n")))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, EncodePktLine([]byte("unshallow "+oid+"\n")))
	}
	if len(plan.Objects) > 0 {
		inputs := make([]PackObjectInput, len(plan.Objects))
		for i, obj := range plan.Objects {
			inputs[i] = PackObjectInput{Type: obj.Type, Data: obj.Data}
		}
		pack := WritePack(inputs)
		parts = append(parts, sidebandPackets(pack)...)
	}
	parts = append(parts, FlushPkt)
	return storeFetchResponse(ctx.Store, key, bytes.Join(parts, nil))
}

// HandleUploadPack handles POST /{owner}/{repo}.git/git-upload-pack:
// v2 when the Git-Protocol header says version=2, else v0.
func HandleUploadPack(ctx *Ctx, w http.ResponseWriter, owner, repoName string) {
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return
	}
	repo := gitRepo(ctx, owner, repoName)
	if repo == nil {
		SendNotFound(w, "Not Found")
		return
	}
	var body []byte
	var err error
	if ctx.Headers.Get("Git-Protocol") == "version=2" {
		var command string
		var args []string
		command, args, err = parseV2Request(ctx.Body)
		if err == nil {
			switch command {
			case "ls-refs":
				body = handleLsRefs(ctx, repo, args)
			case "fetch":
				body = handleFetchV2(ctx, repo, args)
			default:
				err = fmt.Errorf("unknown v2 command %s", JSONText(command))
			}
		}
	} else {
		body = handleFetchV0(ctx, repo, ctx.Body)
	}
	if err != nil {
		SendError(w, http.StatusBadRequest, err.Error())
		return
	}
	SendBytes(w, http.StatusOK, body, uploadResult, nil)
}

// ---------------------------------------------------------- receive-pack

// pushCommand is one ref-update line of a receive-pack request.
type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// parseReceivePack parses a receive-pack body: command lines, then a raw
// packfile. It scans pkt-lines by hand so the byte offset of the packfile
// (the first byte after the command flush) is known exactly.
func parseReceivePack(body []byte) ([]pushCommand, []byte, error) {
	var commands []pushCommand
	off := 0
	for {
		if len(body)-off < 4 {
			return nil, nil, errors.New("receive-pack body ends before the command flush")
		}
		header := string(body[off : off+4])
		if header == "0000" {
			off += 4
			break
		}
		n, err := strconv.ParseInt(header, 16, 32)
		if err != nil || n < 4 || off+int(n) > len(body) {
			return nil, nil, fmt.Errorf("bad pkt-line length %s", JSONText(header))
		}
		if header == "0001" || header == "0002" {
			off += int(n)
			continue
		}
		line := strings.TrimSuffix(string(body[off+4:off+int(n)]), "\n")
		// The first command line carries NUL-separated capabilities.
		if nul := strings.Index(line, "\x00"); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			return nil, nil, fmt.Errorf("bad receive-pack command %s", JSONText(line))
		}
		oldOID, newOID, ref := parts[0], parts[1], parts[2]
		if !IsOID(oldOID) || !IsOID(newOID) {
			return nil, nil, fmt.Errorf("bad object id in %s", JSONText(line))
		}
		if !strings.HasPrefix(ref, "refs/") {
			return nil, nil, fmt.Errorf("bad ref %s", JSONText(ref))
		}
		commands = append(commands, pushCommand{oldOID: oldOID, newOID: newOID, ref: ref})
		off += int(n)
	}
	if len(commands) == 0 {
		return nil, nil, errors.New("receive-pack needs at least one command")
	}
	return commands, body[off:], nil
}

// HandleReceivePack handles POST /{owner}/{repo}.git/git-receive-pack.
func HandleReceivePack(ctx *Ctx, w http.ResponseWriter, owner, repoName string) {
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return
	}
	repo := gitRepo(ctx, owner, repoName)
	if repo == nil {
		SendNotFound(w, "Not Found")
		return
	}
	commands, pack, err := parseReceivePack(ctx.Body)
	if err != nil {
		SendError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Unpack first: every object in the pack is stored, whether or not
	// its ref update is accepted (git unpacks before it updates refs).
	unpackOK := true
	unpackDetail := "ok"
	if len(pack) > 0 {
		read, err := ReadPack(pack, func(oid string) *GitObject {
			if obj, ok := repo.Objects[oid]; ok {
				objCopy := obj
				return &objCopy
			}
			return nil
		})
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range read.Objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
					if obj.Type == ObjCommit {
						ctx.Store.CacheParents(repo, oid, obj.Data)
					}
				}
			}
			// New objects can change any fetch's pack: drop cached packs.
			ctx.Store.InvalidatePackCache()
		}
	}
	// Then apply the ref updates, one report line each.
	unpackLine := "unpack ok\n"
	if !unpackOK {
		unpackLine = "unpack " + unpackDetail + "\n"
	}
	report := []string{unpackLine}
	if unpackOK {
		for _, cmd := range commands {
			current, currentOK := repo.Refs[cmd.ref]
			switch {
			case cmd.oldOID == ZeroOID && cmd.newOID != ZeroOID:
				// Create.
				if currentOK {
					report = append(report, "ng "+cmd.ref+" already exists\n")
				} else if _, ok := repo.Objects[cmd.newOID]; !ok {
					report = append(report, "ng "+cmd.ref+" object not found\n")
				} else {
					repo.Refs[cmd.ref] = cmd.newOID
					report = append(report, "ok "+cmd.ref+"\n")
				}
			case cmd.oldOID != ZeroOID && cmd.newOID != ZeroOID:
				// Update: must fast-forward from the advertised old value.
				if !currentOK {
					report = append(report, "ng "+cmd.ref+" no such ref\n")
				} else if current != cmd.oldOID {
					report = append(report, "ng "+cmd.ref+" non-fast-forward\n")
				} else if _, ok := repo.Objects[cmd.newOID]; !ok {
					report = append(report, "ng "+cmd.ref+" object not found\n")
				} else if cmd.newOID != cmd.oldOID && !ctx.Store.IsAncestor(repo, cmd.oldOID, cmd.newOID) {
					report = append(report, "ng "+cmd.ref+" non-fast-forward\n")
				} else {
					repo.Refs[cmd.ref] = cmd.newOID
					report = append(report, "ok "+cmd.ref+"\n")
				}
			case cmd.oldOID != ZeroOID && cmd.newOID == ZeroOID:
				// Delete.
				if !currentOK {
					report = append(report, "ng "+cmd.ref+" no such ref\n")
				} else {
					delete(repo.Refs, cmd.ref)
					report = append(report, "ok "+cmd.ref+"\n")
				}
			default:
				report = append(report, "ng "+cmd.ref+" invalid command\n")
			}
		}
	}
	// The report-status stream is itself pkt-lined inside band 1: each
	// report line is a pkt-line, then a flush, all inside band-1 sideband
	// packets, then the outer flush. A real client reads pkt-lines out
	// of band 1, so raw text there is a protocol error.
	var inner [][]byte
	for _, line := range report {
		inner = append(inner, EncodePktLine([]byte(line)))
	}
	inner = append(inner, FlushPkt)
	parts := sidebandPackets(bytes.Join(inner, nil))
	parts = append(parts, FlushPkt)
	SendBytes(w, http.StatusOK, bytes.Join(parts, nil), receiveResult, nil)
}
