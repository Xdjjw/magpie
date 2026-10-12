package library

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func repoIn(t *testing.T) map[string]string {
	t.Helper()
	v, err := Read(nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range v.Skills {
		out[s.Name] = s.Repo
	}
	return out
}

// A skill not installed from GitHub is grouped with the repository it
// came from all the same (White Immortal on Discord): the skills CLI's
// lock names it, or the git checkout it's in, whatever the remote's form,
// and never with a user or token the remote had. A skill in ~/.claude kept
// in git (the user's dotfiles) is not from that repository.
func TestSkillRepoTraced(t *testing.T) {
	h := sandbox(t)
	// npx skills add mattpocock/skills: in ~/.agents/skills, in its lock
	skill(t, filepath.Join(h, ".agents/skills/grill-me"), "grill-me", "Grill")
	write(t, filepath.Join(h, ".agents/.skill-lock.json"), `{"version":3,"skills":{
		"grill-me":{"source":"mattpocock/skills","sourceType":"github","sourceUrl":"https://github.com/mattpocock/skills.git","skillPath":"skills/grill-me/SKILL.md"},
		"tdd":{"source":"x","sourceType":"git","sourceUrl":"git@gitlab.com:team/skills.git"}}}`)
	os.MkdirAll(filepath.Join(h, ".claude/skills"), 0o755)
	dirLink(filepath.Join(h, ".agents/skills/grill-me"), filepath.Join(h, ".claude/skills/grill-me"))
	// a clone of the repository, linked from the library
	clone := filepath.Join(h, "code/skills")
	skill(t, filepath.Join(clone, "skills/write-a-prd"), "write-a-prd", "PRD")
	write(t, filepath.Join(clone, ".git/config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://github.com/other/fork.git\n[remote \"origin\"]\r\n\turl = https://me:ghp_secret@github.com/mattpocock/skills.git\r\n")
	// a skill that is a clone of its own, moved in
	skill(t, filepath.Join(h, ".claude/skills/review"), "review", "Review")
	write(t, filepath.Join(h, ".claude/skills/review/.git/config"), "[remote \"origin\"]\n\turl = git@gitlab.com:me/review.git\n")
	// ~/.claude in git: the user's dotfiles
	write(t, filepath.Join(h, ".claude/.git/config"), "[remote \"origin\"]\n\turl = git@github.com:me/dotfiles.git\n")
	skill(t, filepath.Join(h, ".claude/skills/notes"), "notes", "Mine")

	ok(t)(ImportSkills([]string{"grill-me", "review", "notes"}))
	p, err := ProbeSkills(filepath.Join(clone, "skills/write-a-prd"))
	if err != nil || len(p.Candidates) != 1 {
		t.Fatalf("probe: %+v %v", p, err)
	}
	ok(t)(InstallSkills(filepath.Join(clone, "skills/write-a-prd"), []string{p.Candidates[0].Path}, nil))

	got := repoIn(t)
	want := map[string]string{"grill-me": "mattpocock/skills", "write-a-prd": "mattpocock/skills", "review": "gitlab.com/me/review", "notes": ""}
	for n, r := range want {
		if got[n] != r {
			t.Errorf("%s: repo %q, want %q", n, got[n], r)
		}
	}
	b, _ := os.ReadFile(path())
	if strings.Contains(string(b), "ghp_secret") {
		t.Error("the remote's token was written down")
	}
	// the clone moved in kept where it came from
	if s := mustLoad(t).skill("review"); s.From != "gitlab.com/me/review" {
		t.Errorf("review's From: %q", s.From)
	}
}

// The remote forms git takes, each read as the repository.
func TestRemoteRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/mattpocock/skills":          "mattpocock/skills",
		"https://www.github.com/mattpocock/skills.git/": "mattpocock/skills",
		"git@github.com:mattpocock/skills.git":          "mattpocock/skills",
		"ssh://git@github.com/mattpocock/skills.git":    "mattpocock/skills",
		"https://user:tok@github.com/a/b/tree/main/x":   "a/b",
		"git@gitlab.com:group/sub/proj.git":             "gitlab.com/group/sub/proj",
		"https://tok@codeberg.org/me/s.git":             "codeberg.org/me/s",
		"mattpocock/skills":                             "mattpocock/skills",
		"/Users/me/skills":                              "",
		"https://github.com/onlyowner":                  "",
		"https://example.com/../etc":                    "",
	} {
		if got := remoteRepo(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// Installing from a repository again groups the skills the library has
// already from it (White Immortal: installed before, from that very
// address): those whose SKILL.md is the repository's own. One of the same
// name that differs is someone else's skill, and stays where it was.
func TestInstallAgainNotesFrom(t *testing.T) {
	h := sandbox(t)
	newFakeGitHub(t)
	// the fake repository's skills/pdf/SKILL.md, as installed by hand
	write(t, filepath.Join(h, ".claude/skills/pdf/SKILL.md"), "---\nname: pdf\ndescription: PDFs\n---\n")
	ok(t)(ImportSkill("pdf"))
	if r := repoIn(t)["pdf"]; r != "" {
		t.Fatalf("pdf has a repo before: %q", r)
	}
	if _, err := ProbeSkills("someone/fromagain"); err != nil {
		t.Fatal(err)
	}
	if r := repoIn(t)["pdf"]; r != "someone/fromagain" {
		t.Errorf("pdf after installing from its repository again: %q", r)
	}

	// another's pdf, not the same file: not noted
	h = sandbox(t)
	write(t, filepath.Join(h, ".claude/skills/pdf/SKILL.md"), "---\nname: pdf\ndescription: My own\n---\n")
	ok(t)(ImportSkill("pdf"))
	if _, err := ProbeSkills("someone/notmine"); err != nil {
		t.Fatal(err)
	}
	if r := repoIn(t)["pdf"]; r != "" {
		t.Errorf("another's pdf grouped with the repository: %q", r)
	}
}

func mustLoad(t *testing.T) *Library {
	t.Helper()
	l, err := load()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// A pack added a while ago is mostly older copies of its skills (White
// Immortal on Discord: 20 of mattpocock/skills' 38 grouped, the rest
// edited there since). Group again fetches each repository the library
// has skills from, and the skills it has by name that are traced to
// nowhere join the rest of it; one of the user's own it doesn't have, and
// one traced to another repository, stay where they are.
func TestRegroupOlderCopies(t *testing.T) {
	h := sandbox(t)
	repos := map[string]map[string]string{
		"someone/pack": {
			"skills/eng/same/SKILL.md":    "---\nname: same\ndescription: Same\n---\nBody\n",
			"skills/eng/edited/SKILL.md":  "---\nname: edited\ndescription: Edited since\n---\nNew body\n",
			"skills/misc/later/SKILL.md":  "---\nname: later\ndescription: Rewritten\n---\nAll new\n",
			"skills/misc/locked/SKILL.md": "---\nname: locked\ndescription: Theirs\n---\n",
		},
		"other/locked": {"skills/locked/SKILL.md": "---\nname: locked\ndescription: Locked\n---\n"},
	}
	var asked []string
	var amu sync.Mutex
	tar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := strings.TrimPrefix(r.URL.Path, "/tar/")
		amu.Lock()
		asked = append(asked, repo)
		amu.Unlock()
		files, ok := repos[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(tarball(t, files))
	}))
	t.Cleanup(tar.Close)
	oldT := tarballURL
	tarballURL = func(repo, ref string) string { return tar.URL + "/tar/" + repo }
	t.Cleanup(func() { tarballURL = oldT })

	// the copies as added before: same is the repository's own, edited
	// and later older ones, mine the user's own
	write(t, filepath.Join(h, ".claude/skills/same/SKILL.md"), repos["someone/pack"]["skills/eng/same/SKILL.md"])
	skill(t, filepath.Join(h, ".claude/skills/edited"), "edited", "Edited")
	skill(t, filepath.Join(h, ".claude/skills/later"), "later", "Later")
	skill(t, filepath.Join(h, ".claude/skills/mine"), "mine", "Mine")
	// npx skills add other/locked: traced by the lock
	skill(t, filepath.Join(h, ".agents/skills/locked"), "locked", "Locked")
	write(t, filepath.Join(h, ".agents/.skill-lock.json"), `{"version":3,"skills":{"locked":{"source":"other/locked","sourceType":"github"}}}`)
	dirLink(filepath.Join(h, ".agents/skills/locked"), filepath.Join(h, ".claude/skills/locked"))
	ok(t)(ImportSkills([]string{"same", "edited", "later", "mine", "locked"}))
	// same grouped before, as installing from the repository again did
	l := mustLoad(t)
	l.skill("same").From = "someone/pack"
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	if r := repoIn(t)["edited"]; r != "" {
		t.Fatalf("edited has a repo before: %q", r)
	}

	n, err := RegroupSkills()
	if err != nil {
		t.Fatal(err)
	}
	got := repoIn(t)
	want := map[string]string{"same": "someone/pack", "edited": "someone/pack", "later": "someone/pack", "mine": "", "locked": "other/locked"}
	for name, r := range want {
		if got[name] != r {
			t.Errorf("%s: repo %q, want %q", name, got[name], r)
		}
	}
	if n != 2 {
		t.Errorf("grouped %d, want 2", n)
	}
	amu.Lock()
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"other/locked", "someone/pack"}) {
		t.Errorf("fetched %v", asked)
	}
	asked = nil
	amu.Unlock()

	// nothing more to group: none grouped, nothing wrong
	if n, err := RegroupSkills(); err != nil || n != 0 {
		t.Errorf("again: %d %v", n, err)
	}
	// GitHub not answering: said, nothing changed
	tarballURL = func(repo, ref string) string { return tar.URL + "/tar/gone/" + repo }
	if _, err := RegroupSkills(); err == nil {
		t.Error("no repository fetched, and no error")
	}
	if r := repoIn(t)["edited"]; r != "someone/pack" {
		t.Errorf("edited after a failed regroup: %q", r)
	}
}
