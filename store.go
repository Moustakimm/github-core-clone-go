// store.go — the forge's in-memory store: repositories, issues, pull
// requests, and the git graph algorithms (reachability, fetch planning,
// tree diff, merge-base, conflict detection).
//
// Ported from the Node reference (store.ts) into idiomatic Go, stdlib only.
// The store is in-memory only; nothing here reads the wall clock, so a live
// run and its replay leave identical state.

package main

import (
	"encoding/base64"
	"sort"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// constants
// ---------------------------------------------------------------------------

// ZERO_OID is the all-zero object id.
const ZERO_OID = "0000000000000000000000000000000000000000"

// Viewer is the token owner, fixed by the suite's seeded scenario.
const (
	ViewerID    = 1000
	ViewerLogin = "adaiversen"
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
)

// Fixed timestamps: nothing the server stores may read the wall clock.
const FIXED_EPOCH = 1735689600 // 2025-01-01T00:00:00Z
const FIXED_ISO = "2025-01-01T00:00:00.000Z"

// Forge identity.
const FORGE_NAME = "github-core-clone"
const FORGE_VERSION = "1.0.0"

// ---------------------------------------------------------------------------
// types
// ---------------------------------------------------------------------------

// Object is one git object in a repository's object store.
type Object struct {
	Type string // "blob", "tree", "commit" or "tag"
	Data []byte
}

// Comment is one issue comment.
type Comment struct {
	RestID    int
	Body      string
	CreatedAt string
	UpdatedAt string
}

// Issue is a repository issue.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Labels    []string
	Comments  []*Comment
	RestID    int
	NodeID    string
	CreatedAt string
	UpdatedAt string
}

// Pull is a repository pull request.
type Pull struct {
	Number   int
	Title    string
	Body     string
	State    string // "open" or "closed"
	Head     Ref
	Base     Ref
	Merged   bool
	MergedAt string
	RestID   int
	NodeID   string
}

// Ref names a branch (or other ref) by its short name.
type Ref struct {
	Ref string
	SHA string
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Repo is one repository: git objects and refs plus issues, PRs and labels.
type Repo struct {
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string // "main"
	DbID          int
	RestID        int
	NodeID        string
	Refs          map[string]string // refname -> oid
	Objects       map[string]*Object
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	Labels        map[string]*Label
	NextNumber    int // shared issue + PR counter, GitHub style
	NextCommentID int
	Seq           int
	// Revision counters for the response caches. Rev moves on any change a
	// REST reply can show (repo fields, issues, comments, pulls); GitRev
	// moves on any change a git reply can show (objects, refs). A cached
	// reply is only served while its revision still matches.
	Rev    int64
	GitRev int64
	// Serialized replies, keyed by what they show. Built on a miss, served
	// verbatim on a hit while Rev (JSONCache) or GitRev (PackCache) matches.
	JSONCache map[string][]byte
	PackCache map[string][]byte
}

// Store is the forge's whole in-memory state.
type Store struct {
	Repos        map[string]*Repo
	NextDbID     int
	NextObjectID int // global REST `id` counter (repos, issues, PRs, comments)
	RequestSeq   int
	mu           sync.Mutex
}

// Lock acquires the store mutex (serializes all request handling).
func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Repos:        make(map[string]*Repo),
		NextDbID:     1,
		NextObjectID: 1,
	}
}

// ---------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------

// GlobalID returns base64("Kind:id"), e.g. Repository, Issue, PullRequest.
func GlobalID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + itoa(id)))
}

// DecodeGlobalID splits a global id into its kind and the rest after the
// first colon. It returns ok=false for malformed input.
func DecodeGlobalID(gid string) (kind string, rest []string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(gid)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 1 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------

// RepoOptions are the creation options for NewRepo.
type RepoOptions struct {
	Description string
	Private     bool
}

// NewRepo creates a repository record owned by the viewer and registers it
// in the store.
func NewRepo(store *Store, name string, opts RepoOptions) *Repo {
	dbID := store.NextDbID
	store.NextDbID++
	restID := store.NextObjectID
	store.NextObjectID++
	repo := &Repo{
		Name:          name,
		Owner:         ViewerLogin,
		Description:   opts.Description,
		Private:       opts.Private,
		DefaultBranch: "main",
		DbID:          dbID,
		RestID:        restID,
		NodeID:        GlobalID("Repository", dbID),
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Object),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		Labels:        make(map[string]*Label),
		NextNumber:    1,
		NextCommentID: 1,
		Seq:           dbID,
		JSONCache:     make(map[string][]byte),
		PackCache:     make(map[string][]byte),
	}
	store.Repos[name] = repo
	return repo
}

// TouchRepo marks a repository changed: every cached reply built from it
// goes stale.
func TouchRepo(repo *Repo) {
	repo.Rev++
	repo.GitRev++
}

// FindRepo returns the named repository, or nil. A repo under another owner
// is nil (404, never 403).
func FindRepo(store *Store, owner, name string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return store.Repos[name]
}

// RepoObject returns the object with oid from the repo, or nil.
func RepoObject(repo *Repo, oid string) *Object {
	return repo.Objects[oid]
}

// PutObject hashes and stores an object if absent, returning its oid.
func PutObject(repo *Repo, typ string, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &Object{Type: typ, Data: data}
	}
	return oid
}

// PeelToCommit peels a tag (recursively, up to 10 levels) to its commit,
// or returns the oid unchanged.
func PeelToCommit(repo *Repo, oid string) string {
	current := oid
	for i := 0; i < 10; i++ {
		obj := repo.Objects[current]
		if obj == nil || obj.Type != "tag" {
			return current
		}
		current = ParseTag(obj.Data).Object
	}
	return current
}

// RefCommit returns the commit a ref points at (peeling tags), or "".
func RefCommit(repo *Repo, refName string) string {
	oid, ok := repo.Refs[refName]
	if !ok {
		return ""
	}
	return PeelToCommit(repo, oid)
}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

// FlatEntry is one file in a flattened tree: path -> blob oid + mode.
type FlatEntry struct {
	OID  string
	Mode string
}

// FlattenTree flattens a tree to path -> FlatEntry, recursively.
func FlattenTree(repo *Repo, treeOID string, prefix string, out map[string]FlatEntry) map[string]FlatEntry {
	if out == nil {
		out = make(map[string]FlatEntry)
	}
	obj := repo.Objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return out
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return out
	}
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			FlattenTree(repo, e.OID, path, out)
		} else {
			out[path] = FlatEntry{OID: e.OID, Mode: e.Mode}
		}
	}
	return out
}

// TreeFromFlat builds (and stores) a tree from a flat path map and returns
// the tree oid.
func TreeFromFlat(repo *Repo, flat map[string]FlatEntry) string {
	var build func(paths map[string]FlatEntry) string
	build = func(paths map[string]FlatEntry) string {
		var files []TreeEntry
		dirs := make(map[string]map[string]FlatEntry)
		for path, value := range paths {
			slash := strings.IndexByte(path, '/')
			if slash < 0 {
				files = append(files, TreeEntry{Mode: value.Mode, Name: path, OID: value.OID})
			} else {
				dir := path[:slash]
				sub, ok := dirs[dir]
				if !ok {
					sub = make(map[string]FlatEntry)
					dirs[dir] = sub
				}
				sub[path[slash+1:]] = value
			}
		}
		entries := append([]TreeEntry(nil), files...)
		for dir, sub := range dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: build(sub)})
		}
		return PutObject(repo, "tree", BuildTree(entries))
	}
	return build(flat)
}

// FileChange is one entry of a CommitFiles change set: Content nil deletes.
type FileChange struct {
	Path    string
	Content []byte // nil means delete
}

// CommitFiles commits a set of file writes/deletes on a branch. It creates
// blob/tree/commit objects and moves the ref, returning the commit oid.
func CommitFiles(repo *Repo, branch string, changes []FileChange, message string) string {
	refName := "refs/heads/" + branch
	currentTip := RefCommit(repo, refName)
	flat := make(map[string]FlatEntry)
	if currentTip != "" {
		if obj := repo.Objects[currentTip]; obj != nil && obj.Type == "commit" {
			if c, err := ParseCommit(obj.Data); err == nil {
				flat = FlattenTree(repo, c.Tree, "", nil)
			}
		}
	}
	for _, ch := range changes {
		if ch.Content == nil {
			delete(flat, ch.Path)
		} else {
			flat[ch.Path] = FlatEntry{OID: PutObject(repo, "blob", ch.Content), Mode: "100644"}
		}
	}
	treeOID := TreeFromFlat(repo, flat)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	parents := []string{}
	if currentTip != "" {
		parents = []string{currentTip}
	}
	commitOID := PutObject(repo, "commit", BuildCommit(Commit{
		Tree:    treeOID,
		Parents: parents,
		Message: message,
	}))
	repo.Refs[refName] = commitOID
	// A new commit moves the branch: every cached reply built from this
	// repository (REST and git alike) goes stale.
	TouchRepo(repo)
	return commitOID
}

// ---------------------------------------------------------------------------
// commit graph
// ---------------------------------------------------------------------------

// ListCommits lists commits reachable from tip (an oid, a branch name, or a
// full ref name), newest first, up to limit (default 100 when limit <= 0).
func ListCommits(repo *Repo, from string, limit int) []string {
	if limit <= 0 {
		limit = 100
	}
	tip := from
	if !IsObjectID(from) {
		tip = RefCommit(repo, "refs/heads/"+from)
		if tip == "" {
			tip = RefCommit(repo, from)
		}
	}
	if tip == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		out = append(out, oid)
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return out
}

// AncestorSet returns the ancestor set of a commit (including itself).
func AncestorSet(repo *Repo, tip string) map[string]bool {
	set := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == "" || set[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		set[oid] = true
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return set
}

// MergeBase returns the merge-base of two commits: the first ancestor of a
// that is also an ancestor of b, or "".
func MergeBase(repo *Repo, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	bAncestors := AncestorSet(repo, b)
	seen := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		if bAncestors[oid] {
			return oid
		}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return ""
}

// IsAncestor reports whether ancestor is reachable from descendant (or they
// are equal). It walks parents from the descendant and stops at the first
// hit, so the common push case answers after one commit.
func IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	if ancestor == "" || descendant == "" {
		return false
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		stack = append(stack, c.Parents...)
	}
	return false
}

// ---------------------------------------------------------------------------
// tree diff
// ---------------------------------------------------------------------------

// TreeDiff is one entry of a DiffTrees result.
type TreeDiff struct {
	Path      string
	Status    string // "added", "removed" or "modified"
	OldOID    string
	NewOID    string
	Additions int
	Deletions int
	Changes   int
}

// DiffTrees diffs two trees, returning one entry per changed path.
func DiffTrees(repo *Repo, baseTreeOID, headTreeOID string) []TreeDiff {
	base := make(map[string]FlatEntry)
	if baseTreeOID != "" {
		base = FlattenTree(repo, baseTreeOID, "", nil)
	}
	head := make(map[string]FlatEntry)
	if headTreeOID != "" {
		head = FlattenTree(repo, headTreeOID, "", nil)
	}
	pathSet := make(map[string]bool)
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []TreeDiff
	for _, path := range paths {
		b, hasB := base[path]
		h, hasH := head[path]
		if hasB && hasH && b.OID == h.OID {
			continue
		}
		var status string
		switch {
		case !hasB:
			status = "added"
		case !hasH:
			status = "removed"
		default:
			status = "modified"
		}
		additions, deletions := 0, 0
		oldText := ""
		if hasB {
			oldText = BlobText(repo, b.OID)
		}
		newText := ""
		if hasH {
			newText = BlobText(repo, h.OID)
		}
		switch status {
		case "added":
			additions = countLines(newText)
		case "removed":
			deletions = countLines(oldText)
		default:
			additions, deletions = lineDiffCounts(oldText, newText)
		}
		oldOID, newOID := "", ""
		if hasB {
			oldOID = b.OID
		}
		if hasH {
			newOID = h.OID
		}
		out = append(out, TreeDiff{
			Path: path, Status: status, OldOID: oldOID, NewOID: newOID,
			Additions: additions, Deletions: deletions, Changes: additions + deletions,
		})
	}
	return out
}

// BlobText returns a blob's contents as text, or "" when missing/not a blob.
func BlobText(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != "blob" {
		return ""
	}
	return string(obj.Data)
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	lines := strings.Split(text, "\n")
	n := len(lines)
	if lines[n-1] == "" {
		n--
	}
	return n
}

// lineDiffCounts is a cheap line diff: trim common prefix/suffix lines,
// count what is left.
func lineDiffCounts(oldText, newText string) (additions, deletions int) {
	var a, b []string
	if oldText != "" {
		a = strings.Split(oldText, "\n")
	}
	if newText != "" {
		b = strings.Split(newText, "\n")
	}
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	if endB-start > 0 {
		additions = endB - start
	}
	if endA-start > 0 {
		deletions = endA - start
	}
	return additions, deletions
}

// ---------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------

// FetchPlan is the result of PlanFetch: the objects to send plus the shallow
// boundary / deepening information.
type FetchPlan struct {
	Objects   []PackObject // objects to send, each tagged with its oid
	Shallow   []string
	Unshallow []string
}

// PackObject is one object in a fetch plan: type + data + oid.
type PackObject struct {
	Type string
	Data []byte
	OID  string
}

// PlanFetch plans a fetch: which objects to send for wants given the
// client's haves, existing shallow boundaries and an optional depth.
// depth < 0 means no depth limit.
func PlanFetch(repo *Repo, wants, haves, shallow []string, depth int) FetchPlan {
	haveCommits := make(map[string]bool)
	for _, h := range haves {
		obj := repo.Objects[h]
		if obj == nil {
			continue
		}
		if obj.Type == "commit" {
			haveCommits[h] = true
		} else if obj.Type == "tag" {
			target := PeelToCommit(repo, h)
			if repo.Objects[target] != nil && repo.Objects[target].Type == "commit" {
				haveCommits[target] = true
			}
		}
	}
	shallowSet := make(map[string]bool, len(shallow))
	for _, s := range shallow {
		shallowSet[s] = true
	}
	included := make(map[string]bool)
	includedTags := make(map[string]bool)
	boundary := make(map[string]bool)
	deepened := make(map[string]bool)
	visited := make(map[string]bool)
	type queued struct {
		oid string
		d   int
	}
	var queue []queued
	for _, want := range wants {
		obj := repo.Objects[want]
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			includedTags[want] = true
			target := PeelToCommit(repo, want)
			if repo.Objects[target] != nil && repo.Objects[target].Type == "commit" {
				queue = append(queue, queued{target, 1})
			}
		} else if obj.Type == "commit" {
			queue = append(queue, queued{want, 1})
		}
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		oid, d := item.oid, item.d
		if visited[oid] {
			continue
		}
		visited[oid] = true
		if haveCommits[oid] {
			// Already held: stop, unless this is an old shallow boundary
			// being deepened.
			if depth >= 0 && shallowSet[oid] && d < depth {
				deepened[oid] = true
				if obj := repo.Objects[oid]; obj != nil && obj.Type == "commit" {
					if c, err := ParseCommit(obj.Data); err == nil {
						for _, p := range c.Parents {
							queue = append(queue, queued{p, d + 1})
						}
					}
				}
			}
			continue
		}
		included[oid] = true
		if depth >= 0 && d >= depth {
			boundary[oid] = true
			continue
		}
		if obj := repo.Objects[oid]; obj != nil && obj.Type == "commit" {
			if c, err := ParseCommit(obj.Data); err == nil {
				for _, p := range c.Parents {
					queue = append(queue, queued{p, d + 1})
				}
			}
		}
	}
	// Expand each included commit to the objects it needs, without crossing
	// the depth boundary: trees and blobs are followed fully, but a parent
	// commit is followed only when it is itself included (within depth).
	sendSet := make(map[string]bool)
	expanded := make(map[string]bool)
	stack := make([]string, 0, len(included)+len(includedTags))
	for oid := range included {
		stack = append(stack, oid)
	}
	for oid := range includedTags {
		stack = append(stack, oid)
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if expanded[oid] {
			continue
		}
		expanded[oid] = true
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		sendSet[oid] = true
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			if c.Tree != "" {
				stack = append(stack, c.Tree)
			}
			for _, p := range c.Parents {
				if included[p] {
					stack = append(stack, p)
				}
			}
		case "tree":
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				stack = append(stack, e.OID)
			}
		case "tag":
			if target := ParseTag(obj.Data).Object; target != "" {
				stack = append(stack, target)
			}
		}
	}
	haveReachable := heldReachable(repo, haves, shallowSet)
	var oids []string
	for oid := range sendSet {
		if haveReachable[oid] {
			continue
		}
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objects := make([]PackObject, 0, len(oids))
	for _, oid := range oids {
		if obj := repo.Objects[oid]; obj != nil {
			objects = append(objects, PackObject{Type: obj.Type, Data: obj.Data, OID: oid})
		}
	}
	return FetchPlan{
		Objects:   objects,
		Shallow:   sortedKeys(boundary),
		Unshallow: sortedKeys(deepened),
	}
}

// heldReachable is the closure of the client's haves, stopping at the
// shallow boundaries it declared — a boundary commit's parents are not held
// (the client stopped there), so they must still be delivered when a deepen
// asks for them.
func heldReachable(repo *Repo, haves []string, shallowSet map[string]bool) map[string]bool {
	seen := make(map[string]bool)
	queue := append([]string(nil), haves...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == "" || seen[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			if c.Tree != "" {
				queue = append(queue, c.Tree)
			}
			if !shallowSet[oid] {
				queue = append(queue, c.Parents...)
			}
		case "tag":
			if target := ParseTag(obj.Data).Object; target != "" {
				queue = append(queue, target)
			}
		case "tree":
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				queue = append(queue, e.OID)
			}
		}
	}
	return seen
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// pull request conflicts
// ---------------------------------------------------------------------------

// PRConflict reports whether a pull request's head is mergeable into its
// base without conflicts.
func PRConflict(repo *Repo, pr *Pull) bool {
	headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
	baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
	if headTip == "" || baseTip == "" {
		return true
	}
	base := MergeBase(repo, headTip, baseTip)
	if base == "" {
		return false
	}
	baseCommit, err := ParseCommit(repo.Objects[base].Data)
	if err != nil {
		return true
	}
	headCommit, err := ParseCommit(repo.Objects[headTip].Data)
	if err != nil {
		return true
	}
	baseTipCommit, err := ParseCommit(repo.Objects[baseTip].Data)
	if err != nil {
		return true
	}
	headChanges := DiffTrees(repo, baseCommit.Tree, headCommit.Tree)
	baseChanges := make(map[string]TreeDiff)
	for _, d := range DiffTrees(repo, baseCommit.Tree, baseTipCommit.Tree) {
		baseChanges[d.Path] = d
	}
	for _, change := range headChanges {
		other, ok := baseChanges[change.Path]
		if !ok {
			continue
		}
		if other.NewOID != change.NewOID {
			return true
		}
	}
	return false
}
