// The in-memory store: repositories, git objects and refs, issues, pull
// requests, labels and comments. One process, one trial, one fresh store:
// every trial starts empty and the store is never persisted.
//
// The token's owner is the scenario's viewer. The scenario is fixed by the
// kit's public seed, and the viewer is `adaiversen` (id 1000); the token
// itself carries no login, so the forge answers with that login.
//
// Concurrency: Store methods NEVER lock. The handler locks once per
// request (RLock for reads, Lock for writes) via Lock/Unlock/RLock/RUnlock
// below. The caches (isoCache, packCache, per-repo IssueCache) are
// sync.Map so cache writes are safe on a read path. The per-repo
// commitParents map is a plain map guarded by the store lock.
package main

import (
	"sort"
	"strconv"
	"sync"
)

// The login the bearer token names (the scenario's viewer).
const ViewerLogin = "adaiversen"

// The viewer's numeric id (the scenario's viewer id).
const ViewerID = 1000

// The viewer's email, in the scenario's `login@example.invalid` shape.
const ViewerEmail = "adaiversen@example.invalid"

// The default branch every repository is born with.
const DefaultBranch = "main"

type Label struct {
	Name, Color, Description string
}

type Comment struct {
	ID         int
	Body       string
	User       string
	CreatedSeq int
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string
	Labels     []string
	Comments   []Comment
	CreatedSeq int
	UpdatedSeq int
	User       string
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	HeadRef        string
	HeadSha        string
	BaseRef        string
	BaseSha        string
	CreatedSeq     int
	User           string
}

type Repo struct {
	Owner           string
	Name            string
	ID              int
	Description     string
	Private         bool
	DefaultBranch   string
	CreatedSeq      int
	Objects         map[string]GitObject
	Refs            map[string]string
	Issues          []*Issue
	Pulls           []*Pull
	Labels          map[string]Label
	NextIssueNumber int
	NextPullNumber  int
	NextCommentID   int
	IssueCache      sync.Map // int -> string (serialized issue JSON)
	// Parsed parents of each commit object, by id. Commit objects are
	// immutable, so once parsed the parents never change; ancestor walks
	// read this instead of re-parsing the same commit buffers over and
	// over. Guarded by the store lock, never accessed without it.
	CommitParents map[string][]string
}

type FlatFile struct {
	OID  string
	Mode string
}

type Store struct {
	mu            sync.RWMutex
	repos         map[string]*Repo
	nextRepoID    int
	nextIssueID   int
	nextPullID    int
	nextCommentID int
	seq           int
	isoCache      sync.Map // int64 -> string
	packCache     sync.Map // string -> []byte
}

type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// NewStore returns a fresh empty store with the scenario's id counters.
func NewStore() *Store {
	s := &Store{repos: make(map[string]*Repo)}
	s.initCounters()
	return s
}

// initCounters sets the starting id counters. Called by NewStore and
// lazily by NextSeq so a zero-value Store still behaves.
func (s *Store) initCounters() {
	if s.repos == nil {
		s.repos = make(map[string]*Repo)
	}
	if s.nextRepoID == 0 {
		s.nextRepoID = 10000
	}
	if s.nextIssueID == 0 {
		s.nextIssueID = 20000
	}
	if s.nextPullID == 0 {
		s.nextPullID = 40000
	}
	if s.nextCommentID == 0 {
		s.nextCommentID = 30000
	}
}

func (s *Store) Lock()    { s.mu.Lock() }
func (s *Store) Unlock()  { s.mu.Unlock() }
func (s *Store) RLock()   { s.mu.RLock() }
func (s *Store) RUnlock() { s.mu.RUnlock() }

// NextSeq returns the next sequence number (1-based).
func (s *Store) NextSeq() int {
	s.initCounters()
	s.seq += 1
	return s.seq
}

// NowSec is the deterministic "now" for a new event, in seconds since
// the epoch.
func (s *Store) NowSec() int64 {
	return s.TimeFor(s.NextSeq())
}

// IsoFor returns the ISO timestamp for a sequence number, cached.
func (s *Store) IsoFor(seq int) string {
	return s.IsoSec(s.TimeFor(seq))
}

// IsoSec returns the ISO timestamp for an epoch second, cached (no
// time formatting per call).
func (s *Store) IsoSec(sec int64) string {
	if cached, ok := s.isoCache.Load(sec); ok {
		return cached.(string)
	}
	iso := IsoOf(sec)
	s.isoCache.Store(sec, iso)
	return iso
}

// TimeFor returns a deterministic timestamp for sequence `seq`
// (seconds since the epoch).
func (s *Store) TimeFor(seq int) int64 {
	return TrialNowSec() + int64(seq)
}

func (s *Store) Key(owner, name string) string {
	return owner + "/" + name
}

func (s *Store) GetRepo(owner, name string) *Repo {
	if s.repos == nil {
		return nil
	}
	return s.repos[s.Key(owner, name)]
}

func (s *Store) ListViewerRepos() []*Repo {
	out := []*Repo{}
	for _, repo := range s.repos {
		if repo.Owner == ViewerLogin {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

func (s *Store) CreateRepo(owner, name, description string, isPrivate bool) *Repo {
	s.initCounters()
	if _, exists := s.repos[s.Key(owner, name)]; exists {
		return nil
	}
	repo := &Repo{
		Owner:           owner,
		Name:            name,
		ID:              s.nextRepoID,
		Description:     description,
		Private:         isPrivate,
		DefaultBranch:   DefaultBranch,
		CreatedSeq:      s.NextSeq(),
		Objects:         make(map[string]GitObject),
		Refs:            make(map[string]string),
		Issues:          []*Issue{},
		Pulls:           []*Pull{},
		Labels:          make(map[string]Label),
		NextIssueNumber: 1,
		NextPullNumber:  1,
		NextCommentID:   1,
		CommitParents:   make(map[string][]string),
	}
	s.nextRepoID += 1
	s.repos[s.Key(owner, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	if _, ok := s.repos[s.Key(owner, name)]; !ok {
		return false
	}
	delete(s.repos, s.Key(owner, name))
	return true
}

// ---------------------------------------------------------- git objects

func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	obj, ok := repo.Objects[oid]
	if !ok {
		return nil
	}
	out := obj
	return &out
}

func (s *Store) HasObject(repo *Repo, oid string) bool {
	_, ok := repo.Objects[oid]
	return ok
}

// PutObject stores an object, deduplicated by id, and returns its id.
func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
		if typ == ObjCommit {
			s.CacheParents(repo, oid, data)
		}
		// A new object can change any fetch's pack, so drop cached packs.
		s.packCache.Clear()
	}
	return oid
}

// CacheParents records a commit's parents in the cache from its raw
// bytes. Called whenever a commit object is stored (PutObject, or a pack
// unpack that writes objects directly), so ancestor walks never parse it
// again.
func (s *Store) CacheParents(repo *Repo, oid string, data []byte) {
	if _, ok := repo.CommitParents[oid]; !ok {
		repo.CommitParents[oid] = ParseCommit(data).Parents
	}
}

// ParentsOf returns a commit's parents, from the cache when present,
// else parsed once and cached. Anything that is not a stored commit has
// no parents.
func (s *Store) ParentsOf(repo *Repo, oid string) []string {
	if cached, ok := repo.CommitParents[oid]; ok {
		return cached
	}
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != ObjCommit {
		return []string{}
	}
	parents := ParseCommit(obj.Data).Parents
	if repo.CommitParents == nil {
		repo.CommitParents = make(map[string][]string)
	}
	repo.CommitParents[oid] = parents
	return parents
}

// InvalidatePackCache drops all cached packs (call after a push adds
// objects directly).
func (s *Store) InvalidatePackCache() {
	s.packCache.Clear()
}

// HeadOID is the tip of the default branch, or false when the repo is
// unborn.
func (s *Store) HeadOID(repo *Repo) (string, bool) {
	oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	return oid, ok
}

func (s *Store) RefOID(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func (s *Store) SetRef(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
}

func (s *Store) DeleteRef(repo *Repo, ref string) bool {
	if _, ok := repo.Refs[ref]; !ok {
		return false
	}
	delete(repo.Refs, ref)
	return true
}

// PeelToCommit peels a tag object to its commit, following nested tags.
func (s *Store) PeelToCommit(repo *Repo, oid string) (string, bool) {
	current := oid
	for hops := 0; hops < 16; hops++ {
		obj, ok := repo.Objects[current]
		if !ok {
			return "", false
		}
		if obj.Type == ObjCommit {
			return current, true
		}
		if obj.Type != ObjTag {
			return "", false
		}
		current = ParseTag(obj.Data).Object
	}
	return "", false
}

// IsAncestor reports whether `ancestor` is an ancestor of (or equal to)
// `descendant`.
func (s *Store) IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		for _, parent := range s.ParentsOf(repo, oid) {
			stack = append(stack, parent)
		}
	}
	return false
}

// ancestorsOrdered returns all ancestors of `oid` (including itself)
// with their depth from it, plus the BFS discovery order of the ids.
// The order matters: MergeBase breaks ties by first discovery, matching
// the reference's insertion-ordered map walk.
func (s *Store) ancestorsOrdered(repo *Repo, oid string) (map[string]int, []string) {
	depths := make(map[string]int)
	order := []string{}
	type queued struct {
		oid   string
		depth int
	}
	queue := []queued{{oid, 0}}
	for head := 0; head < len(queue); head++ {
		current := queue[head].oid
		depth := queue[head].depth
		if _, seen := depths[current]; seen {
			continue
		}
		depths[current] = depth
		order = append(order, current)
		for _, parent := range s.ParentsOf(repo, current) {
			if _, seen := depths[parent]; !seen {
				queue = append(queue, queued{parent, depth + 1})
			}
		}
	}
	return depths, order
}

// Ancestors returns all ancestors of `oid` (including itself) with
// their depth from it.
func (s *Store) Ancestors(repo *Repo, oid string) map[string]int {
	depths, _ := s.ancestorsOrdered(repo, oid)
	return depths
}

// MergeBase returns the merge base of two commits: the common ancestor
// closest to both.
func (s *Store) MergeBase(repo *Repo, a, b string) (string, bool) {
	fromA, orderA := s.ancestorsOrdered(repo, a)
	fromB, _ := s.ancestorsOrdered(repo, b)
	best := ""
	bestDepth := -1
	for _, oid := range orderA {
		depthB, ok := fromB[oid]
		if !ok {
			continue
		}
		depth := fromA[oid] + depthB
		if bestDepth < 0 || depth < bestDepth {
			bestDepth = depth
			best = oid
		}
	}
	if bestDepth < 0 {
		return "", false
	}
	return best, true
}

// FlatTree flattens a commit's tree into path -> file, recursing into
// subtrees.
func (s *Store) FlatTree(repo *Repo, commitOID string) map[string]FlatFile {
	out := make(map[string]FlatFile)
	obj, ok := repo.Objects[commitOID]
	if !ok || obj.Type != ObjCommit {
		return out
	}
	root := ParseCommit(obj.Data).Tree
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		tree, ok := repo.Objects[treeOID]
		if !ok || tree.Type != ObjTree {
			return
		}
		for _, entry := range ParseTree(tree.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else if entry.Mode != "160000" {
				out[path] = FlatFile{OID: entry.OID, Mode: entry.Mode}
			}
		}
	}
	walk(root, "")
	return out
}

// BuildTreeFromFlat builds nested trees from a flat file map, storing
// every tree object, and returns the root tree id. Entries are sorted
// in git's tree order by BuildTree.
func (s *Store) BuildTreeFromFlat(repo *Repo, flat map[string]FlatFile) string {
	// Group files by directory, bottom-up.
	dirs := make(map[string]map[string]FlatFile)
	subdirs := make(map[string]map[string]bool)
	for path, file := range flat {
		slash := lastSlash(path)
		dir := ""
		name := path
		if slash >= 0 {
			dir = path[:slash]
			name = path[slash+1:]
		}
		entries, ok := dirs[dir]
		if !ok {
			entries = make(map[string]FlatFile)
			dirs[dir] = entries
		}
		entries[name] = file
		// Record every ancestor directory.
		parent := dir
		for parent != "" {
			at := lastSlash(parent)
			up := ""
			base := parent
			if at >= 0 {
				up = parent[:at]
				base = parent[at+1:]
			}
			set, ok := subdirs[up]
			if !ok {
				set = make(map[string]bool)
				subdirs[up] = set
			}
			set[base] = true
			parent = up
		}
	}
	// Depth of a directory: number of slashes + 1 ('' is 0).
	depthOf := func(dir string) int {
		if dir == "" {
			return 0
		}
		n := 1
		for i := 0; i < len(dir); i++ {
			if dir[i] == '/' {
				n++
			}
		}
		return n
	}
	seen := make(map[string]bool)
	allDirs := []string{}
	for dir := range dirs {
		if !seen[dir] {
			seen[dir] = true
			allDirs = append(allDirs, dir)
		}
	}
	for dir := range subdirs {
		if !seen[dir] {
			seen[dir] = true
			allDirs = append(allDirs, dir)
		}
	}
	sort.SliceStable(allDirs, func(i, j int) bool { return depthOf(allDirs[i]) > depthOf(allDirs[j]) })
	treeOIDOf := make(map[string]string)
	for _, dir := range allDirs {
		entries := []TreeEntry{}
		if files, ok := dirs[dir]; ok {
			for name, file := range files {
				entries = append(entries, TreeEntry{Mode: file.Mode, Name: name, OID: file.OID})
			}
		}
		if subs, ok := subdirs[dir]; ok {
			for name := range subs {
				child := name
				if dir != "" {
					child = dir + "/" + name
				}
				if oid, ok := treeOIDOf[child]; ok {
					entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: oid})
				}
			}
		}
		treeOIDOf[dir] = s.PutObject(repo, ObjTree, BuildTree(entries))
	}
	if root, ok := treeOIDOf[""]; ok {
		return root
	}
	// An empty tree.
	return s.PutObject(repo, ObjTree, BuildTree([]TreeEntry{}))
}

// lastSlash returns the index of the last '/' in s, or -1.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// CreateCommit creates a commit object (stored) and returns its id.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string, timestampSec *int64) string {
	var ts int64
	if timestampSec != nil {
		ts = *timestampSec
	} else {
		ts = s.TimeFor(s.NextSeq())
	}
	data := BuildCommit(CommitInput{
		Tree:      tree,
		Parents:   parents,
		Message:   message,
		Author:    Identity{Name: ViewerLogin, Email: ViewerEmail},
		Committer: Identity{Name: ViewerLogin, Email: ViewerEmail},
		Timestamp: ts,
	})
	return s.PutObject(repo, ObjCommit, data)
}

// ---------------------------------------------------------- three-way merge

// ThreeWayMerge three-way merges `headOID` into `baseOID` over their
// merge base. It returns the merged flat tree and whether any path
// conflicts (both sides changed it differently). A path changed on one
// side only wins; a path changed identically on both wins; anything else
// conflicts.
func (s *Store) ThreeWayMerge(repo *Repo, baseOID, headOID string) (map[string]FlatFile, bool) {
	base, hasBase := s.MergeBase(repo, baseOID, headOID)
	baseFlat := make(map[string]FlatFile)
	if hasBase {
		baseFlat = s.FlatTree(repo, base)
	}
	headFlat := s.FlatTree(repo, headOID)
	tipFlat := s.FlatTree(repo, baseOID)
	merged := make(map[string]FlatFile)
	conflict := false
	same := func(x, y *FlatFile) bool {
		if x == nil && y == nil {
			return true
		}
		if x == nil || y == nil {
			return false
		}
		return x.OID == y.OID && x.Mode == y.Mode
	}
	paths := make(map[string]bool)
	for path := range baseFlat {
		paths[path] = true
	}
	for path := range headFlat {
		paths[path] = true
	}
	for path := range tipFlat {
		paths[path] = true
	}
	for path := range paths {
		var b, h, t *FlatFile
		if v, ok := baseFlat[path]; ok {
			vv := v
			b = &vv
		}
		if v, ok := headFlat[path]; ok {
			vv := v
			h = &vv
		}
		if v, ok := tipFlat[path]; ok {
			vv := v
			t = &vv
		}
		if same(h, b) {
			// Unchanged on the head side: keep the base side.
			if t != nil {
				merged[path] = *t
			}
		} else if same(t, b) {
			// Unchanged on the base side: take the head side.
			if h != nil {
				merged[path] = *h
			}
		} else if same(h, t) {
			// Both changed identically.
			if h != nil {
				merged[path] = *h
			}
		} else {
			conflict = true
		}
	}
	return merged, conflict
}

// Mergeable reports whether a pull request's head can merge cleanly
// into its base.
func (s *Store) Mergeable(repo *Repo, pr *Pull) bool {
	head, ok := repo.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return false
	}
	base, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return false
	}
	_, conflict := s.ThreeWayMerge(repo, base, head)
	return !conflict
}

// ---------------------------------------------------------- issues and pulls

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	seq := s.NextSeq()
	labelsCopy := make([]string, len(labels))
	copy(labelsCopy, labels)
	issue := &Issue{
		ID:         s.nextIssueID,
		Number:     repo.NextIssueNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     labelsCopy,
		Comments:   []Comment{},
		CreatedSeq: seq,
		UpdatedSeq: seq,
		User:       ViewerLogin,
	}
	s.nextIssueID += 1
	repo.NextIssueNumber += 1
	repo.Issues = append(repo.Issues, issue)
	repo.IssueCache.Delete(issue.Number)
	return issue
}

func (s *Store) CreatePull(repo *Repo, title, body, headRef, headSha, baseRef, baseSha string) *Pull {
	seq := s.NextSeq()
	pr := &Pull{
		ID:             s.nextPullID,
		Number:         repo.NextPullNumber,
		Title:          title,
		Body:           body,
		State:          "open",
		Merged:         false,
		MergedAt:       nil,
		MergeCommitSha: nil,
		HeadRef:        headRef,
		HeadSha:        headSha,
		BaseRef:        baseRef,
		BaseSha:        baseSha,
		CreatedSeq:     seq,
		User:           ViewerLogin,
	}
	s.nextPullID += 1
	repo.NextPullNumber += 1
	repo.Pulls = append(repo.Pulls, pr)
	return pr
}

func (s *Store) FindIssue(repo *Repo, number int) *Issue {
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

func (s *Store) FindPull(repo *Repo, number int) *Pull {
	for _, pr := range repo.Pulls {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

// AddComment adds a comment to an issue and returns the comment.
func (s *Store) AddComment(repo *Repo, issue *Issue, body string) Comment {
	comment := Comment{
		ID:         repo.NextCommentID,
		Body:       body,
		User:       ViewerLogin,
		CreatedSeq: s.NextSeq(),
	}
	repo.NextCommentID += 1
	issue.Comments = append(issue.Comments, comment)
	s.TouchIssue(repo, issue)
	return comment
}

// ---------------------------------------------------------- global ids

func (s *Store) RepoGlobalID(repo *Repo) string {
	return B64URL("R:" + repo.Owner + "/" + repo.Name)
}

func (s *Store) IssueGlobalID(repo *Repo, issue *Issue) string {
	return B64URL("I:" + repo.Owner + "/" + repo.Name + ":" + strconv.Itoa(issue.Number))
}

func (s *Store) PullGlobalID(repo *Repo, pr *Pull) string {
	return B64URL("P:" + repo.Owner + "/" + repo.Name + ":" + strconv.Itoa(pr.Number))
}

func (s *Store) CommentGlobalID(repo *Repo, issue *Issue, comment Comment) string {
	return B64URL("C:" + repo.Owner + "/" + repo.Name + ":" + strconv.Itoa(issue.Number) + ":" + strconv.Itoa(comment.ID))
}

// ResolveGlobalID resolves an opaque global id back to its object, or
// ("", nil, nil, nil) when not found.
func (s *Store) ResolveGlobalID(id string) (string, *Repo, *Issue, *Pull) {
	for _, repo := range s.repos {
		if id == s.RepoGlobalID(repo) {
			return "repo", repo, nil, nil
		}
		for _, issue := range repo.Issues {
			if id == s.IssueGlobalID(repo, issue) {
				return "issue", repo, issue, nil
			}
		}
		for _, pr := range repo.Pulls {
			if id == s.PullGlobalID(repo, pr) {
				return "pull", repo, nil, pr
			}
		}
	}
	return "", nil, nil, nil
}

// ---------------------------------------------------------- JSON shapes

func (s *Store) RepoJSON(repo *Repo) map[string]any {
	created := s.IsoFor(repo.CreatedSeq)
	ownerID := 0
	if repo.Owner == ViewerLogin {
		ownerID = ViewerID
	}
	return map[string]any{
		"id":             repo.ID,
		"node_id":        s.RepoGlobalID(repo),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner, "id": ownerID, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"fork":           false,
		"created_at":     created,
		"updated_at":     created,
		"pushed_at":      created,
		"default_branch": repo.DefaultBranch,
		"has_issues":     true,
		"has_projects":   false,
		"has_wiki":       false,
		"archived":       false,
		"disabled":       false,
		"visibility":     visibilityOf(repo.Private),
	}
}

func visibilityOf(isPrivate bool) string {
	if isPrivate {
		return "private"
	}
	return "public"
}

func (s *Store) LabelJSON(label Label) map[string]any {
	return map[string]any{
		"name":        label.Name,
		"color":       label.Color,
		"description": label.Description,
		"default":     false,
	}
}

func userJSON(login string) map[string]any {
	id := 0
	if login == ViewerLogin {
		id = ViewerID
	}
	return map[string]any{"login": login, "id": id, "type": "User"}
}

func (s *Store) IssueJSON(repo *Repo, issue *Issue) map[string]any {
	labels := []map[string]any{}
	for _, name := range issue.Labels {
		if label, ok := repo.Labels[name]; ok {
			labels = append(labels, s.LabelJSON(label))
		}
	}
	return map[string]any{
		"id":         issue.ID,
		"node_id":    s.IssueGlobalID(repo, issue),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"locked":     false,
		"user":       userJSON(issue.User),
		"labels":     labels,
		"comments":   len(issue.Comments),
		"created_at": s.IsoFor(issue.CreatedSeq),
		"updated_at": s.IsoFor(issue.UpdatedSeq),
	}
}

// IssueJSONText returns the cached serialized issue JSON (invalidated
// on mutation).
func (s *Store) IssueJSONText(repo *Repo, issue *Issue) string {
	if cached, ok := repo.IssueCache.Load(issue.Number); ok {
		return cached.(string)
	}
	text := JSONText(s.IssueJSON(repo, issue))
	repo.IssueCache.Store(issue.Number, text)
	return text
}

func (s *Store) TouchIssue(repo *Repo, issue *Issue) {
	issue.UpdatedSeq = s.NextSeq()
	repo.IssueCache.Delete(issue.Number)
}

func (s *Store) CommentJSON(repo *Repo, issue *Issue, comment Comment) map[string]any {
	return map[string]any{
		"id":         comment.ID,
		"node_id":    s.CommentGlobalID(repo, issue, comment),
		"body":       comment.Body,
		"user":       userJSON(comment.User),
		"created_at": s.IsoFor(comment.CreatedSeq),
		"updated_at": s.IsoFor(comment.CreatedSeq),
	}
}

func (s *Store) PullJSON(repo *Repo, pr *Pull) map[string]any {
	var mergeable any
	if pr.State == "open" {
		mergeable = s.Mergeable(repo, pr)
	} else {
		mergeable = nil
	}
	// Unwrap the *string fields: a typed nil *string stored in an
	// any would compare non-nil even though it marshals as null.
	var mergedAt any
	if pr.MergedAt != nil {
		mergedAt = *pr.MergedAt
	}
	var mergeCommitSha any
	if pr.MergeCommitSha != nil {
		mergeCommitSha = *pr.MergeCommitSha
	}
	return map[string]any{
		"id":               pr.ID,
		"node_id":          s.PullGlobalID(repo, pr),
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            pr.State,
		"locked":           false,
		"user":             userJSON(pr.User),
		"head":             map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSha, "label": repo.Owner + ":" + pr.HeadRef},
		"base":             map[string]any{"ref": pr.BaseRef, "sha": pr.BaseSha, "label": repo.Owner + ":" + pr.BaseRef},
		"merged":           pr.Merged,
		"mergeable":        mergeable,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"created_at":       s.IsoFor(pr.CreatedSeq),
		"updated_at":       s.IsoFor(pr.CreatedSeq),
	}
}

// PullFiles returns the changed files of a pull request against its
// merge base.
func (s *Store) PullFiles(repo *Repo, pr *Pull) []PullFile {
	base, hasBase := s.MergeBase(repo, pr.BaseSha, pr.HeadSha)
	baseFlat := make(map[string]FlatFile)
	if hasBase {
		baseFlat = s.FlatTree(repo, base)
	}
	headFlat := s.FlatTree(repo, pr.HeadSha)
	out := []PullFile{}
	paths := make(map[string]bool)
	for path := range baseFlat {
		paths[path] = true
	}
	for path := range headFlat {
		paths[path] = true
	}
	for path := range paths {
		b, inBase := baseFlat[path]
		h, inHead := headFlat[path]
		if inBase && inHead && b.OID == h.OID {
			continue
		}
		if !inBase && inHead {
			out = append(out, PullFile{Filename: path, Status: "added", Additions: 1, Deletions: 0, Changes: 1})
		} else if inBase && !inHead {
			out = append(out, PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: 1, Changes: 1})
		} else {
			out = append(out, PullFile{Filename: path, Status: "modified", Additions: 1, Deletions: 1, Changes: 2})
		}
	}
	sort.Slice(out, func(i, j int) bool { return CmpStr(out[i].Filename, out[j].Filename) < 0 })
	return out
}
