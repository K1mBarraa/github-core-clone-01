package main

import (
	"encoding/json"
	"testing"
)

// The gitobj/util stubs return empty values until builders A1/B land
// the real implementations. Skip the tests that need them when the
// stubs are still in place, so the suite stays green before integration
// and exercises the real code after it.
func needRealGitobj(t *testing.T) {
	t.Helper()
	if HashObject(ObjBlob, []byte("probe")) == "" {
		t.Skip("gitobj stub still in place (HashObject returns empty)")
	}
}

func needRealUtil(t *testing.T) {
	t.Helper()
	if IsoOf(1735689600) == "" || B64URL("probe") == "" || JSONText("x") == "null" {
		t.Skip("util stub still in place")
	}
}

func newTestRepo(t *testing.T, s *Store) *Repo {
	t.Helper()
	repo := s.CreateRepo(ViewerLogin, "demo", "a demo repo", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	return repo
}

// makeCommit builds a commit on top of parents with the given files
// (path -> content) and returns its oid. Files are stored as blobs.
func makeCommit(t *testing.T, s *Store, repo *Repo, parents []string, files map[string]string) string {
	t.Helper()
	flat := make(map[string]FlatFile)
	for path, content := range files {
		oid := s.PutObject(repo, ObjBlob, []byte(content))
		flat[path] = FlatFile{OID: oid, Mode: "100644"}
	}
	tree := s.BuildTreeFromFlat(repo, flat)
	return s.CreateCommit(repo, tree, parents, "test commit", nil)
}

func TestCreateRepoAndList(t *testing.T) {
	s := NewStore()
	if s.NextSeq() != 1 || s.NextSeq() != 2 {
		t.Fatal("NextSeq did not count 1, 2")
	}
	if got := s.TimeFor(5); got != TrialNowSec()+5 {
		t.Fatalf("TimeFor(5) = %d, want %d", got, TrialNowSec()+5)
	}
	r1 := s.CreateRepo(ViewerLogin, "a", "", false)
	if r1 == nil || r1.ID != 10000 || r1.CreatedSeq == 0 {
		t.Fatalf("bad first repo: %+v", r1)
	}
	if s.CreateRepo(ViewerLogin, "a", "", false) != nil {
		t.Fatal("CreateRepo duplicate should return nil")
	}
	r2 := s.CreateRepo(ViewerLogin, "b", "desc", true)
	if r2 == nil || r2.ID != 10001 || !r2.Private {
		t.Fatalf("bad second repo: %+v", r2)
	}
	// Other-owner repos are not listed for the viewer.
	s.CreateRepo("someone", "c", "", false)
	list := s.ListViewerRepos()
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("ListViewerRepos = %v, want [a b]", list)
	}
	if got := s.GetRepo(ViewerLogin, "b"); got != r2 {
		t.Fatal("GetRepo did not return r2")
	}
	if s.GetRepo(ViewerLogin, "nope") != nil {
		t.Fatal("GetRepo missing should return nil")
	}
	if !s.DeleteRepo(ViewerLogin, "b") || s.DeleteRepo(ViewerLogin, "b") {
		t.Fatal("DeleteRepo should be true once, false twice")
	}
	if len(s.ListViewerRepos()) != 1 {
		t.Fatal("list should have 1 repo after delete")
	}
}

func TestRefs(t *testing.T) {
	s := NewStore()
	repo := newTestRepo(t, s)
	if _, ok := s.HeadOID(repo); ok {
		t.Fatal("HeadOID on unborn repo should be absent")
	}
	s.SetRef(repo, "refs/heads/main", "abc")
	if oid, ok := s.HeadOID(repo); !ok || oid != "abc" {
		t.Fatalf("HeadOID = %q %v", oid, ok)
	}
	if oid, ok := s.RefOID(repo, "refs/heads/main"); !ok || oid != "abc" {
		t.Fatal("RefOID mismatch")
	}
	if !s.DeleteRef(repo, "refs/heads/main") || s.DeleteRef(repo, "refs/heads/main") {
		t.Fatal("DeleteRef should be true once, false twice")
	}
}

func TestIssuePullComment(t *testing.T) {
	s := NewStore()
	repo := newTestRepo(t, s)
	repo.Labels["bug"] = Label{Name: "bug", Color: "ff0000", Description: "a bug"}
	iss := s.CreateIssue(repo, "Title", "Body", []string{"bug", "missing"})
	if iss == nil || iss.ID != 20000 || iss.Number != 1 || iss.State != "open" {
		t.Fatalf("bad issue: %+v", iss)
	}
	iss2 := s.CreateIssue(repo, "Second", "", nil)
	if iss2.ID != 20001 || iss2.Number != 2 {
		t.Fatalf("bad second issue: %+v", iss2)
	}
	if s.FindIssue(repo, 1) != iss || s.FindIssue(repo, 99) != nil {
		t.Fatal("FindIssue mismatch")
	}
	before := iss.UpdatedSeq
	c := s.AddComment(repo, iss, "hello")
	if c.ID != 1 || c.Body != "hello" || c.User != ViewerLogin {
		t.Fatalf("bad comment: %+v", c)
	}
	if len(iss.Comments) != 1 || iss.UpdatedSeq <= before {
		t.Fatal("AddComment should append and bump updatedSeq")
	}
	pr := s.CreatePull(repo, "PR", "", "feature", "headsha", "main", "basesha")
	if pr == nil || pr.ID != 40000 || pr.Number != 1 || pr.Merged {
		t.Fatalf("bad pull: %+v", pr)
	}
	if s.FindPull(repo, 1) != pr || s.FindPull(repo, 7) != nil {
		t.Fatal("FindPull mismatch")
	}
}

func TestGlobalIDs(t *testing.T) {
	needRealUtil(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	iss := s.CreateIssue(repo, "T", "", nil)
	pr := s.CreatePull(repo, "P", "", "f", "h", "main", "b")
	c := s.AddComment(repo, iss, "c")
	// Round-trip through ResolveGlobalID.
	for _, tc := range []struct {
		kind string
		id   string
	}{
		{"repo", s.RepoGlobalID(repo)},
		{"issue", s.IssueGlobalID(repo, iss)},
		{"pull", s.PullGlobalID(repo, pr)},
	} {
		kind, r, is, p := s.ResolveGlobalID(tc.id)
		if kind != tc.kind || r != repo {
			t.Fatalf("ResolveGlobalID(%q) = %q, want %q", tc.id, kind, tc.kind)
		}
		if tc.kind == "issue" && is != iss {
			t.Fatal("resolved issue mismatch")
		}
		if tc.kind == "pull" && p != pr {
			t.Fatal("resolved pull mismatch")
		}
	}
	// Comment ids are not resolvable (TS has no comment case either).
	if kind, _, _, _ := s.ResolveGlobalID(s.CommentGlobalID(repo, iss, c)); kind != "" {
		t.Fatalf("comment global id should not resolve, got %q", kind)
	}
	if kind, _, _, _ := s.ResolveGlobalID("bogus"); kind != "" {
		t.Fatal("bogus id should not resolve")
	}
	// The encoded payloads are the R:/I:/P: strings.
	if got := s.RepoGlobalID(repo); got != B64URL("R:adaiversen/demo") {
		t.Fatalf("repo global id = %q, want B64URL(R:adaiversen/demo)", got)
	}
	if got := s.IssueGlobalID(repo, iss); got != B64URL("I:adaiversen/demo:1") {
		t.Fatalf("issue global id = %q, want B64URL(I:adaiversen/demo:1)", got)
	}
}

func TestCommitChainMergeBase(t *testing.T) {
	needRealGitobj(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	c1 := makeCommit(t, s, repo, nil, map[string]string{"a.txt": "one"})
	c2 := makeCommit(t, s, repo, []string{c1}, map[string]string{"a.txt": "two"})
	c3 := makeCommit(t, s, repo, []string{c2}, map[string]string{"a.txt": "three"})
	if !s.IsAncestor(repo, c1, c3) || !s.IsAncestor(repo, c2, c2) {
		t.Fatal("IsAncestor failed on linear chain")
	}
	if s.IsAncestor(repo, c3, c1) {
		t.Fatal("IsAncestor reversed should be false")
	}
	mb, ok := s.MergeBase(repo, c2, c3)
	if !ok || mb != c2 {
		t.Fatalf("MergeBase(c2,c3) = %q %v, want %q", mb, ok, c2)
	}
	mb, ok = s.MergeBase(repo, c1, c3)
	if !ok || mb != c1 {
		t.Fatalf("MergeBase(c1,c3) = %q %v, want %q", mb, ok, c1)
	}
	anc := s.Ancestors(repo, c3)
	if len(anc) != 3 || anc[c3] != 0 || anc[c2] != 1 || anc[c1] != 2 {
		t.Fatalf("Ancestors(c3) = %v", anc)
	}
	// commitParents cache is populated by PutObject.
	if _, ok := repo.CommitParents[c2]; !ok {
		t.Fatal("commitParents cache missing c2")
	}
}

func TestFlatTreeRoundTrip(t *testing.T) {
	needRealGitobj(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	files := map[string]string{
		"a.txt":        "root",
		"dir/b.txt":    "nested",
		"dir/sub/c.md": "deep",
	}
	c := makeCommit(t, s, repo, nil, files)
	flat := s.FlatTree(repo, c)
	if len(flat) != 3 {
		t.Fatalf("FlatTree = %d files, want 3: %v", len(flat), flat)
	}
	for path, content := range files {
		f, ok := flat[path]
		if !ok {
			t.Fatalf("FlatTree missing %s", path)
		}
		obj := s.GetObject(repo, f.OID)
		if obj == nil || string(obj.Data) != content {
			t.Fatalf("blob for %s mismatch", path)
		}
	}
	tree := s.BuildTreeFromFlat(repo, flat)
	c2 := s.CreateCommit(repo, tree, nil, "rebuilt", nil)
	if got := s.FlatTree(repo, c2); len(got) != 3 {
		t.Fatalf("round-trip FlatTree = %d files, want 3", len(got))
	}
	// Empty flat map builds the empty tree.
	empty := s.BuildTreeFromFlat(repo, map[string]FlatFile{})
	if len(s.FlatTree(repo, s.CreateCommit(repo, empty, nil, "empty", nil))) != 0 {
		t.Fatal("empty tree should flatten to no files")
	}
}

func TestThreeWayMerge(t *testing.T) {
	needRealGitobj(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	base := makeCommit(t, s, repo, nil, map[string]string{"a.txt": "base", "b.txt": "base"})
	// Head changes a.txt only; tip changes b.txt only -> clean merge.
	head := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "head", "b.txt": "base"})
	tip := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "base", "b.txt": "tip"})
	merged, conflict := s.ThreeWayMerge(repo, tip, head)
	if conflict {
		t.Fatal("disjoint changes should merge cleanly")
	}
	if merged["a.txt"].OID == "" || merged["b.txt"].OID == "" {
		t.Fatalf("merged tree missing files: %v", merged)
	}
	headBlob := s.GetObject(repo, merged["a.txt"].OID)
	tipBlob := s.GetObject(repo, merged["b.txt"].OID)
	if string(headBlob.Data) != "head" || string(tipBlob.Data) != "tip" {
		t.Fatal("merged contents are not the changed sides")
	}
	// Conflict: both sides change a.txt differently.
	head2 := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "head2", "b.txt": "base"})
	tip2 := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "tip2", "b.txt": "base"})
	if _, conflict := s.ThreeWayMerge(repo, tip2, head2); !conflict {
		t.Fatal("both-changed-differently should conflict")
	}
	// Both sides change a.txt identically -> clean, takes the change.
	head3 := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "same", "b.txt": "base"})
	tip3 := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "same", "b.txt": "base"})
	merged, conflict = s.ThreeWayMerge(repo, tip3, head3)
	if conflict {
		t.Fatal("identical changes should merge cleanly")
	}
	if string(s.GetObject(repo, merged["a.txt"].OID).Data) != "same" {
		t.Fatal("identical change not taken")
	}
}

func TestMergeableAndPullFiles(t *testing.T) {
	needRealGitobj(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	base := makeCommit(t, s, repo, nil, map[string]string{"a.txt": "base", "b.txt": "base", "gone.txt": "gone"})
	head := makeCommit(t, s, repo, []string{base}, map[string]string{"a.txt": "head", "b.txt": "base", "new.txt": "new"})
	s.SetRef(repo, "refs/heads/main", base)
	s.SetRef(repo, "refs/heads/feature", head)
	pr := s.CreatePull(repo, "PR", "", "feature", head, "main", base)
	if !s.Mergeable(repo, pr) {
		t.Fatal("disjoint PR should be mergeable")
	}
	files := s.PullFiles(repo, pr)
	if len(files) != 3 {
		t.Fatalf("PullFiles = %v, want 3 entries", files)
	}
	// Sorted by filename: a.txt modified, gone.txt removed, new.txt added.
	if files[0].Filename != "a.txt" || files[0].Status != "modified" || files[0].Changes != 2 {
		t.Fatalf("files[0] = %+v", files[0])
	}
	if files[1].Filename != "gone.txt" || files[1].Status != "removed" || files[1].Deletions != 1 {
		t.Fatalf("files[1] = %+v", files[1])
	}
	if files[2].Filename != "new.txt" || files[2].Status != "added" || files[2].Additions != 1 {
		t.Fatalf("files[2] = %+v", files[2])
	}
}

func TestJSONShapes(t *testing.T) {
	needRealUtil(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	repo.Labels["bug"] = Label{Name: "bug", Color: "d73a4a", Description: "bug"}
	iss := s.CreateIssue(repo, "Title", "Body", []string{"bug", "nope"})
	j := s.IssueJSON(repo, iss)
	if j["number"] != 1 || j["title"] != "Title" || j["state"] != "open" {
		t.Fatalf("IssueJSON = %v", j)
	}
	labels, ok := j["labels"].([]map[string]any)
	if !ok || len(labels) != 1 || labels[0]["name"] != "bug" {
		t.Fatalf("IssueJSON labels = %v (missing label dropped, want 1)", j["labels"])
	}
	if j["comments"] != 0 {
		t.Fatalf("comments = %v, want 0", j["comments"])
	}
	// Cached text is stable, then invalidated by a comment.
	text1 := s.IssueJSONText(repo, iss)
	if text1 != s.IssueJSONText(repo, iss) {
		t.Fatal("IssueJSONText not cached")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text1), &parsed); err != nil {
		t.Fatalf("IssueJSONText is not valid JSON: %v", err)
	}
	s.AddComment(repo, iss, "hi")
	if s.IssueJSONText(repo, iss) == text1 {
		t.Fatal("IssueJSONText should change after a comment")
	}
	rj := s.RepoJSON(repo)
	if rj["full_name"] != "adaiversen/demo" || rj["default_branch"] != "main" || rj["visibility"] != "public" {
		t.Fatalf("RepoJSON = %v", rj)
	}
	pr := s.CreatePull(repo, "PR", "", "feature", "h", "main", "b")
	pj := s.PullJSON(repo, pr)
	if pj["mergeable"] != false {
		t.Fatalf("PullJSON mergeable with missing refs = %v, want false", pj["mergeable"])
	}
	if pj["merged_at"] != nil || pj["merge_commit_sha"] != nil {
		t.Fatal("unmerged PR should have nil merged_at / merge_commit_sha")
	}
	lj := s.LabelJSON(Label{Name: "x", Color: "fff", Description: "d"})
	if lj["default"] != false || lj["color"] != "fff" {
		t.Fatalf("LabelJSON = %v", lj)
	}
	c := iss.Comments[0]
	cj := s.CommentJSON(repo, iss, c)
	if cj["body"] != "hi" {
		t.Fatalf("CommentJSON = %v", cj)
	}
}

func TestPeelToCommit(t *testing.T) {
	needRealGitobj(t)
	s := NewStore()
	repo := newTestRepo(t, s)
	c := makeCommit(t, s, repo, nil, map[string]string{"a.txt": "x"})
	if got, ok := s.PeelToCommit(repo, c); !ok || got != c {
		t.Fatal("PeelToCommit on a commit should return itself")
	}
	if _, ok := s.PeelToCommit(repo, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("PeelToCommit on a missing object should fail")
	}
	tag := s.PutObject(repo, ObjTag, BuildTag(TagInput{
		Object: c, Type: ObjCommit, Tag: "v1",
		Tagger:    Identity{Name: ViewerLogin, Email: ViewerEmail},
		Timestamp: TrialNowSec(), Message: "v1",
	}))
	if got, ok := s.PeelToCommit(repo, tag); !ok || got != c {
		t.Fatalf("PeelToCommit(tag) = %q %v, want %q", got, ok, c)
	}
}
