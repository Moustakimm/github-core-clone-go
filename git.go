// git.go — git smart HTTP: info/refs advertisements (v0 and protocol v2),
// ls-refs, fetch (v2 and v0) and receive-pack (v0), all over the in-memory
// store.
//
// Ported from the Node reference (git.ts) into idiomatic Go, stdlib only.
// The suite's client (kit sdk/conformance/git-client.ts) is the only peer:
// every framing choice below mirrors what it parses.

package main

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
	"strings"
)

const (
	uploadAdvertisement  = "application/x-git-upload-pack-advertisement"
	uploadResult         = "application/x-git-upload-pack-result"
	receiveAdvertisement = "application/x-git-receive-pack-advertisement"
	receiveResult        = "application/x-git-receive-pack-result"
)

// ---------------------------------------------------------------------------
// ref helpers
// ---------------------------------------------------------------------------

type gitRef struct {
	name string
	oid  string
}

func sortedRefs(repo *Repo) []gitRef {
	refs := make([]gitRef, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, gitRef{name: name, oid: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func defaultBranchTip(repo *Repo) string {
	return repo.Refs["refs/heads/"+repo.DefaultBranch]
}

// peeledOf returns the commit an annotated tag peels to, or "" when oid is
// not a tag object.
func peeledOf(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != "tag" {
		return ""
	}
	return PeelToCommit(repo, oid)
}

// mustPktLine frames payload as one pkt-line; the payloads here are short
// and always legal, so an error is impossible.
func mustPktLine(payload string) []byte {
	line, err := EncodePktLine([]byte(payload))
	if err != nil {
		panic(err)
	}
	return line
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

// v0Advertisement builds the v0 ref advertisement body for a service.
func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k multi_ack multi_ack_detailed ofs-delta thin-pack shallow agent=" + FORGE_NAME + "/" + FORGE_VERSION + " symref=HEAD:refs/heads/" + repo.DefaultBranch
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + FORGE_NAME + "/" + FORGE_VERSION + " symref=HEAD:refs/heads/" + repo.DefaultBranch
	}
	var parts [][]byte
	parts = append(parts, mustPktLine("# service="+service+"\n"))
	parts = append(parts, PktFlush())
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		parts = append(parts, mustPktLine(ZERO_OID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		headOid := defaultBranchTip(repo)
		if headOid == "" {
			headOid = ZERO_OID
		}
		parts = append(parts, mustPktLine(headOid+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			parts = append(parts, mustPktLine(ref.oid+" "+ref.name+"\n"))
			if peeled := peeledOf(repo, ref.oid); peeled != "" {
				parts = append(parts, mustPktLine(peeled+" "+ref.name+"^{}\n"))
			}
		}
	}
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

// v2AdvertisementBytes is the protocol v2 capability advertisement body
// (upload-pack only). Constant for the life of the process.
var v2AdvertisementBytes = func() []byte {
	var parts [][]byte
	parts = append(parts, mustPktLine("version 2\n"))
	parts = append(parts, mustPktLine("agent="+FORGE_NAME+"/"+FORGE_VERSION+"\n"))
	parts = append(parts, mustPktLine("ls-refs=unborn\n"))
	parts = append(parts, mustPktLine("fetch=shallow filter\n"))
	parts = append(parts, mustPktLine("object-format=sha1\n"))
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}()

// ---------------------------------------------------------------------------
// request parsing
// ---------------------------------------------------------------------------

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// parseReceiveLines scans pkt-lines only up to the first flush; everything
// after it is the raw pack.
func parseReceiveLines(body []byte) (lines []string, rest []byte, err error) {
	offset := 0
	for offset+4 <= len(body) {
		head := string(body[offset : offset+4])
		if !isHex4(head) {
			return nil, nil, errors.New("invalid pkt-line length")
		}
		length, _ := strconv.ParseInt(head, 16, 32)
		switch {
		case length == 0:
			offset += 4
			return lines, body[offset:], nil
		case length == 1 || length == 2:
			offset += 4
			continue
		}
		if length < 4 || length > PKT_LINE_MAX {
			return nil, nil, errors.New("invalid pkt-line length")
		}
		if offset+int(length) > len(body) {
			return nil, nil, errors.New("truncated pkt-line")
		}
		lines = append(lines, string(body[offset+4:offset+int(length)]))
		offset += int(length)
	}
	return nil, nil, errors.New("no flush packet")
}

// v2Request is a parsed protocol v2 command request.
type v2Request struct {
	command string
	args    []string
}

// parseV2Request parses a v2 command request: {command, args: [lines after
// the command line]}. Delimiter packets are skipped; parsing stops at the
// first flush.
func parseV2Request(body []byte) (*v2Request, error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return nil, err
	}
	var lines []string
scan:
	for _, packet := range packets {
		switch packet.Kind {
		case "delim":
			continue
		case "flush":
			break scan
		case "data":
			lines = append(lines, string(packet.Payload))
		}
	}
	command := ""
	if len(lines) > 0 {
		commandLine := strings.TrimSuffix(lines[0], "\n")
		command = strings.TrimPrefix(commandLine, "command=")
		if command == commandLine {
			command = ""
		}
	}
	var args []string
	for _, line := range lines[1:] {
		args = append(args, strings.TrimSuffix(line, "\n"))
	}
	return &v2Request{command: command, args: args}, nil
}

// ---------------------------------------------------------------------------
// ls-refs
// ---------------------------------------------------------------------------

func lsRefsResponse(repo *Repo, args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	matchPrefix := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var parts [][]byte
	refs := sortedRefs(repo)
	headTip := defaultBranchTip(repo)
	if len(refs) == 0 {
		if unborn && matchPrefix("HEAD") {
			parts = append(parts, mustPktLine("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
		}
	} else {
		if matchPrefix("HEAD") {
			oid := headTip
			if oid == "" {
				oid = ZERO_OID
			}
			line := oid + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			parts = append(parts, mustPktLine(line+"\n"))
		}
		for _, ref := range refs {
			if !matchPrefix(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if peel {
				if peeled := peeledOf(repo, ref.oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
			parts = append(parts, mustPktLine(line+"\n"))
		}
	}
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

// fetchResponse returns a fetch reply, cached per repository revision: a
// fetch reply is a pure function of the repository at one revision and the
// request's wants/haves/shallow/depth, so identical shallow clones of the
// same tip share one built pack.
func fetchResponse(repo *Repo, wants, haves, shallow []string, depth int, protocol int, done bool) ([]byte, error) {
	doneFlag := "0"
	if done {
		doneFlag = "1"
	}
	depthStr := ""
	if depth >= 0 {
		depthStr = strconv.Itoa(depth)
	}
	key := strings.Join([]string{
		strconv.FormatInt(repo.GitRev, 10),
		strconv.Itoa(protocol), doneFlag,
		strings.Join(wants, ","), strings.Join(haves, ","),
		strings.Join(shallow, ","), depthStr,
	}, "|")
	if cached, ok := repo.PackCache[key]; ok {
		return cached, nil
	}
	built, err := buildFetchResponse(repo, wants, haves, shallow, depth, protocol, done)
	if err != nil {
		return nil, err
	}
	if len(repo.PackCache) >= 16 {
		repo.PackCache = make(map[string][]byte)
	}
	repo.PackCache[key] = built
	return built, nil
}

func buildFetchResponse(repo *Repo, wants, haves, shallow []string, depth int, protocol int, done bool) ([]byte, error) {
	plan := PlanFetch(repo, wants, haves, shallow, depth)
	objects := make([]*Object, 0, len(plan.Objects))
	for i := range plan.Objects {
		objects = append(objects, &Object{Type: plan.Objects[i].Type, Data: plan.Objects[i].Data})
	}
	pack, err := WritePack(objects)
	if err != nil {
		return nil, err
	}
	var parts [][]byte
	if protocol == 2 {
		if done {
			// The client sent `done`: negotiation is over, so the response
			// starts with shallow-info (if any) and then the packfile. An
			// `acknowledgments` section here would break real git clients,
			// which go straight to the pack after sending `done`.
			if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
				parts = append(parts, mustPktLine("shallow-info\n"))
				for _, oid := range plan.Shallow {
					parts = append(parts, mustPktLine("shallow "+oid+"\n"))
				}
				for _, oid := range plan.Unshallow {
					parts = append(parts, mustPktLine("unshallow "+oid+"\n"))
				}
				parts = append(parts, PktFlush())
			}
			// Always send the packfile section (a valid empty pack when
			// there is nothing new): real git requires the section header
			// after `done`.
			parts = append(parts, mustPktLine("packfile\n"))
			parts = append(parts, SidebandPack(pack))
			parts = append(parts, PktFlush())
		} else {
			// Negotiation is still open (no `done`): answer NAK and let
			// the client send more haves or `done`. No packfile yet.
			parts = append(parts, mustPktLine("acknowledgments\n"))
			parts = append(parts, mustPktLine("NAK\n"))
			parts = append(parts, PktFlush())
		}
	} else {
		parts = append(parts, mustPktLine("NAK\n"))
		for _, oid := range plan.Shallow {
			parts = append(parts, mustPktLine("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			parts = append(parts, mustPktLine("unshallow "+oid+"\n"))
		}
		parts = append(parts, SidebandPack(pack))
		parts = append(parts, PktFlush())
	}
	return bytes.Join(parts, nil), nil
}

// v2FetchArgs is the parsed argument set of a v2 fetch command.
type v2FetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // -1 means no depth limit
	done    bool
}

func parseV2FetchArgs(args []string) v2FetchArgs {
	f := v2FetchArgs{depth: -1}
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			f.wants = append(f.wants, strings.TrimSpace(arg[len("want "):]))
		case strings.HasPrefix(arg, "have "):
			f.haves = append(f.haves, strings.TrimSpace(arg[len("have "):]))
		case strings.HasPrefix(arg, "shallow "):
			f.shallow = append(f.shallow, strings.TrimSpace(arg[len("shallow "):]))
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(arg[len("deepen "):])); err == nil {
				f.depth = n
			}
		case arg == "done":
			f.done = true
		}
	}
	return f
}

// v0FetchArgs is the parsed argument set of a v0 upload-pack request.
type v0FetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // -1 means no depth limit
}

func parseV0FetchBody(body []byte) (v0FetchArgs, error) {
	// Two sections split by flush: wants (+shallow/deepen), then haves + done.
	packets, err := DecodePktLines(body)
	if err != nil {
		return v0FetchArgs{depth: -1}, err
	}
	sections := [][]string{{}}
	for _, packet := range packets {
		switch packet.Kind {
		case "flush":
			sections = append(sections, []string{})
		case "data":
			last := len(sections) - 1
			sections[last] = append(sections[last], strings.TrimSuffix(string(packet.Payload), "\n"))
		}
	}
	f := v0FetchArgs{depth: -1}
	for _, line := range sections[0] {
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[len("want "):])
			if i := strings.IndexByte(rest, ' '); i >= 0 {
				rest = rest[:i]
			}
			f.wants = append(f.wants, rest)
		case strings.HasPrefix(line, "shallow "):
			f.shallow = append(f.shallow, strings.TrimSpace(line[len("shallow "):]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[len("deepen "):])); err == nil {
				f.depth = n
			}
		}
	}
	if len(sections) > 1 {
		for _, line := range sections[1] {
			if strings.HasPrefix(line, "have ") {
				f.haves = append(f.haves, strings.TrimSpace(line[len("have "):]))
			}
		}
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(c *reqCtx, repo *Repo) {
	lines, rest, err := parseReceiveLines(c.body)
	if err != nil {
		c.jsonErr(400, "Malformed request")
		return
	}
	var commands []pushCommand
	for _, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\n")
		if line == "" {
			continue
		}
		head := line
		if i := strings.IndexByte(line, 0); i >= 0 {
			head = line[:i]
		}
		parts := strings.Split(head, " ")
		if len(parts) < 3 || !IsObjectID(parts[0]) || !IsObjectID(parts[1]) {
			c.jsonErr(400, "Malformed command line")
			return
		}
		commands = append(commands, pushCommand{
			oldOid: parts[0],
			newOid: parts[1],
			ref:    strings.Join(parts[2:], " "),
		})
	}
	if len(commands) == 0 {
		c.jsonErr(400, "No commands")
		return
	}
	// Unpack the pack (if any) first; objects land even for refused refs.
	unpackOk := true
	unpackDetail := "ok"
	hasUpdates := false
	for _, cmd := range commands {
		if cmd.newOid != ZERO_OID {
			hasUpdates = true
			break
		}
	}
	if len(rest) > 0 {
		pack, err := ReadPack(rest)
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else if !pack.ChecksumOK {
			unpackOk = false
			unpackDetail = "pack checksum mismatch"
		} else {
			for oid, object := range pack.Objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = &Object{Type: object.Type, Data: object.Data}
				}
			}
		}
	} else if hasUpdates {
		// A bare command list with updates and no pack: git calls this an
		// unpacker error, but the suite always sends a pack; treat an
		// absent pack as an empty one.
		unpackOk = true
	}
	var reportLines []string
	if unpackOk {
		reportLines = append(reportLines, "unpack ok\n")
	} else {
		reportLines = append(reportLines, "unpack "+unpackDetail+"\n")
	}
	for _, cmd := range commands {
		current, exists := repo.Refs[cmd.ref]
		ok := false
		reason := ""
		switch {
		case !unpackOk:
			reason = "unpack failed"
		case cmd.newOid == ZERO_OID:
			// delete
			switch {
			case !exists:
				reason = "remote ref does not exist"
			case cmd.oldOid != current:
				reason = "non-fast-forward"
			default:
				delete(repo.Refs, cmd.ref)
				ok = true
			}
		case cmd.oldOid == ZERO_OID:
			// create
			switch {
			case exists:
				reason = "already exists"
			case repo.Objects[cmd.newOid] == nil:
				reason = "object not found"
			default:
				repo.Refs[cmd.ref] = cmd.newOid
				ok = true
			}
		default:
			// update
			switch {
			case !exists:
				reason = "remote ref does not exist"
			case cmd.oldOid != current:
				reason = "non-fast-forward"
			case repo.Objects[cmd.newOid] == nil:
				reason = "object not found"
			case !IsAncestor(repo, PeelToCommit(repo, current), PeelToCommit(repo, cmd.newOid)):
				reason = "non-fast-forward"
			default:
				repo.Refs[cmd.ref] = cmd.newOid
				ok = true
			}
		}
		if ok {
			reportLines = append(reportLines, "ok "+cmd.ref+"\n")
		} else {
			reportLines = append(reportLines, "ng "+cmd.ref+" "+reason+"\n")
		}
	}
	// The report travels as sideband band-1 data, and that data is itself a
	// pkt-line stream: the report lines plus a terminating flush *inside*
	// band 1, then the outer flush. Without the inner flush, real git reads
	// the report lines and then hits EOF ("the remote end hung up").
	// Objects and refs may have changed (an unpack lands objects even for
	// refused refs), so every cached git reply built from this repository
	// goes stale.
	TouchRepo(repo)
	var inner [][]byte
	for _, line := range reportLines {
		inner = append(inner, mustPktLine(line))
	}
	inner = append(inner, PktFlush())
	body := append(SidebandPack(bytes.Join(inner, nil)), PktFlush()...)
	c.raw(200, body, receiveResult)
}

// ---------------------------------------------------------------------------
// handler
// ---------------------------------------------------------------------------

// handleGit handles every git smart-HTTP route: /{owner}/{repo}.git/*.
// The router holds the store lock for the whole request, so this handler
// never locks the store itself.
func handleGit(c *reqCtx) {
	path := strings.Trim(c.r.URL.Path, "/")
	seg := strings.Split(path, "/")
	if len(seg) < 3 {
		c.jsonErr(404, "Not Found")
		return
	}
	owner := seg[0]
	repoName := ""
	if strings.HasSuffix(seg[1], ".git") {
		repoName = seg[1][:len(seg[1])-4]
	}
	if repoName == "" {
		c.jsonErr(404, "Not Found")
		return
	}
	tail := seg[2:]
	repo := FindRepo(c.s, owner, repoName)
	service := c.r.URL.Query().Get("service")

	// GET /{owner}/{repo}.git/info/refs?service=...
	if c.r.Method == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		if repo == nil {
			c.jsonErr(404, "Repository not found")
			return
		}
		if service != "git-upload-pack" && service != "git-receive-pack" {
			c.jsonErr(404, "Not Found")
			return
		}
		wantsV2 := service == "git-upload-pack" && c.r.Header.Get("Git-Protocol") == "version=2"
		contentType := uploadAdvertisement
		if service == "git-receive-pack" {
			contentType = receiveAdvertisement
		}
		var body []byte
		if wantsV2 {
			body = v2AdvertisementBytes
		} else {
			body = v0Advertisement(repo, service)
		}
		c.raw(200, body, contentType)
		return
	}

	// POST /{owner}/{repo}.git/git-upload-pack
	if c.r.Method == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		if repo == nil {
			c.jsonErr(404, "Repository not found")
			return
		}
		isV2 := c.r.Header.Get("Git-Protocol") == "version=2"
		if isV2 {
			parsed, err := parseV2Request(c.body)
			if err != nil {
				c.jsonErr(400, "Malformed request")
				return
			}
			switch parsed.command {
			case "ls-refs":
				c.raw(200, lsRefsResponse(repo, parsed.args), uploadResult)
			case "fetch":
				f := parseV2FetchArgs(parsed.args)
				body, err := fetchResponse(repo, f.wants, f.haves, f.shallow, f.depth, 2, f.done)
				if err != nil {
					c.jsonErr(400, err.Error())
					return
				}
				c.raw(200, body, uploadResult)
			default:
				c.jsonErr(400, "Unknown command "+parsed.command)
			}
			return
		}
		f, err := parseV0FetchBody(c.body)
		if err != nil {
			c.jsonErr(400, err.Error())
			return
		}
		body, err := fetchResponse(repo, f.wants, f.haves, f.shallow, f.depth, 0, true)
		if err != nil {
			c.jsonErr(400, err.Error())
			return
		}
		c.raw(200, body, uploadResult)
		return
	}

	// POST /{owner}/{repo}.git/git-receive-pack
	if c.r.Method == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		if repo == nil {
			c.jsonErr(404, "Repository not found")
			return
		}
		handleReceivePack(c, repo)
		return
	}

	c.jsonErr(404, "Not Found")
}
