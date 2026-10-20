//go:build e2e

// Command fakegh is the tier-1 E2E fake for the `gh` CLI (#47). The real worker
// invokes `gh` as a subprocess; this shim is placed on PATH and reads/writes a
// mutable JSON store in FAKE_GH_STATE so scenarios can drive GitHub-like state
// across worker calls (issues, PRs, labels, reviews, comments).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type ghItem struct {
	Number      int       `json:"number"`
	State       string    `json:"state"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Labels      []string  `json:"labels"`
	HeadRef     string    `json:"headRef"`
	IsPR        bool      `json:"isPr"`
	HeadRefOID  string    `json:"headRefOid"`
	Diff        string    `json:"diff"`
	Reviews     []string  `json:"reviews"`
	Comments    []string  `json:"comments"`
	PullRequest bool      `json:"pullRequest"`
	Checks      []ghCheck `json:"checks"`
}

// ghCheck mirrors the subset of `gh pr checks --json bucket,name` we consume.
type ghCheck struct {
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
}

type store struct {
	Issues   []ghItem `json:"issues"`
	Labels   []string `json:"labels"`
	Next     int      `json:"next"`
	CiBucket string   `json:"ciBucket"` // §49: bucket new PRs report (pass default)
}

func statePath() string {
	return filepath.Join(os.Getenv("FAKE_GH_STATE"), "store.json")
}

func realHeadSHA(branch string) string {
	remote := os.Getenv("FAKE_GIT_REMOTE")
	if remote == "" || branch == "" {
		return branch
	}
	out, err := exec.Command("git", "--git-dir", filepath.Join(remote, "o", "r.git"),
		"rev-parse", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakegh pr view: resolve %s: %s\n", branch, strings.TrimSpace(string(out)))
		return branch
	}
	return strings.TrimSpace(string(out))
}

func load() *store {
	s := &store{Next: 1, Labels: []string{"enhancement", "bug"}}
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, s)
	}
	return s
}

func (s *store) save() {
	os.MkdirAll(os.Getenv("FAKE_GH_STATE"), 0o755)
	b, _ := json.Marshal(s)
	os.WriteFile(statePath(), b, 0o644)
}

func fail(format string, a ...interface{}) {
	fmt.Fprintln(os.Stderr, format, a)
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(0)
	}
	s := load()
	switch args[0] {
	case "issue":
		handleIssue(args[1:], s)
	case "pr":
		handlePR(args[1:], s)
	case "label":
		handleLabel(args[1:], s)
	case "api":
		handleAPI(args[1:], s)
	}
}

// ---- issue ----
func handleIssue(args []string, s *store) {
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "view":
		// gh issue view N -R o/r --json title,body,comments
		num := atoi(args[1])
		out := map[string]interface{}{"title": "", "body": "", "comments": []string{}}
		for _, it := range s.Issues {
			if it.Number == num && !it.IsPR {
				out["title"] = it.Title
				out["body"] = it.Body
				cmts := make([]map[string]string, 0, len(it.Comments))
				for _, c := range it.Comments {
					cmts = append(cmts, map[string]string{"body": c})
				}
				out["comments"] = cmts
			}
		}
		writeJSON(out)
	case "list":
		// gh issue list -R o/r --state open --json number,labels --jq ".[] | select(...) .number"
		var nums []string
		for _, it := range s.Issues {
			if !it.IsPR && it.State == "open" {
				if flagHas(args, "--jq") {
					nums = append(nums, strconv.Itoa(it.Number))
				}
			}
		}
		if flagHas(args, "--jq") {
			// jq faked: emit numbers (ListUnmanagedIssues filters konoha:managed
			// itself as select(...); for tier-1 we treat "unlabelled" as managed-
			// missing → return open issues that lack konoha:managed).
			fmt.Println(strings.Join(nums, "\n"))
			return
		}
		var items []map[string]interface{}
		for _, it := range s.Issues {
			if !it.IsPR && it.State == "open" {
				items = append(items, map[string]interface{}{
					"number": it.Number,
					"labels": it.Labels,
				})
			}
		}
		writeJSON(items)
	case "create":
		it := ghItem{Number: s.Next, State: "open", IsPR: false}
		it.Title = flagVal(args, "--title")
		it.Body = flagVal(args, "--body")
		for i := 0; i < len(args); i++ {
			if args[i] == "--label" && i+1 < len(args) {
				it.Labels = append(it.Labels, args[i+1])
			}
		}
		s.Next++
		s.Issues = append(s.Issues, it)
		s.save()
		fmt.Printf("https://github.com/o/r/issues/%d\n", it.Number)
	case "edit":
		// gh issue edit <num> -R o/r [--state|--add-label|--remove-label] ...
		num := atoi(args[1])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--state":
				s.Issues[idx].State = args[i+1]
			case "--add-label":
				l := args[i+1]
				if !contains(s.Issues[idx].Labels, l) {
					s.Issues[idx].Labels = append(s.Issues[idx].Labels, l)
				}
			case "--remove-label":
				l := args[i+1]
				s.Issues[idx].Labels = remove(s.Issues[idx].Labels, l)
			}
		}
		s.save()
	}
}

// ---- pr ----
func handlePR(args []string, s *store) {
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		// gh pr list -R o/r --head b --state open --json number,url
		head := flagVal(args, "--head")
		var items []map[string]interface{}
		for _, it := range s.Issues {
			if it.IsPR && it.HeadRef == head && it.State == "open" {
				items = append(items, map[string]interface{}{"number": it.Number, "url": fmt.Sprintf("https://github.com/o/r/pull/%d", it.Number)})
			}
		}
		if flagHas(args, "--jq") {
			// jq is faked: first element "number url"
			if len(items) > 0 {
				fmt.Printf("%d %s\n", items[0]["number"], items[0]["url"])
			}
			return
		}
		writeJSON(items)
	case "view":
		// gh pr view N -R o/r --json headRefOid --jq .headRefOid
		// gh pr view N -R o/r --json state            (§49 merge probe)
		num := atoi(args[1])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		it := s.Issues[idx]
		if strings.Contains(flagVal(args, "--json"), "state") {
			writeJSON(map[string]interface{}{"state": strings.ToUpper(it.State)})
			return
		}
		// Resolve the real pushed commit SHA from the fixture bare remote, so
		// RunRepair's `git reset --hard <sha>` gets a real object (#47).
		sha := realHeadSHA(it.HeadRef)
		if flagHas(args, "--jq") {
			fmt.Println(sha) // jq .headRefOid faked: emit the SHA only
		} else {
			writeJSON(map[string]interface{}{"headRefOid": sha})
		}
	case "checks":
		// gh pr checks N -R o/r --json bucket,name   (§49 CI gate)
		num := atoi(args[1])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		checks := s.Issues[idx].Checks
		if len(checks) == 0 {
			checks = []ghCheck{{Bucket: "pass", Name: "e2e"}}
		}
		writeJSON(checks)
	case "merge":
		// gh pr merge N -R o/r [--squash|--rebase|--merge] [--match-head-commit SHA]
		num := atoi(args[1])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		s.Issues[idx].State = "merged"
		s.save()
		fmt.Printf("Merged pull request #%d\n", num)
	case "diff":
		num := atoi(args[0])
		for _, it := range s.Issues {
			if it.IsPR && it.Number == num {
				fmt.Print(it.Diff)
				return
			}
		}
	case "edit":
		// gh pr edit N -R o/r --title .. --body ..
		num := atoi(args[1])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		for i := 0; i < len(args); i++ {
			if args[i] == "--title" && i+1 < len(args) {
				s.Issues[idx].Title = args[i+1]
			}
			if args[i] == "--body" && i+1 < len(args) {
				s.Issues[idx].Body = flagBodyVal(args, i)
			}
		}
		s.save()
		os.Exit(0)
	case "review":
		num := atoi(args[0])
		idx := findIssue(s, num)
		if idx < 0 {
			os.Exit(1)
		}
		if flagHas(args, "--approve") {
			s.Issues[idx].Reviews = append(s.Issues[idx].Reviews, "APPROVED")
		} else if flagHas(args, "--request-changes") {
			s.Issues[idx].Reviews = append(s.Issues[idx].Reviews, "REQUESTED_CHANGES")
		}
		s.save()
	case "create":
		// gh pr create -R o/r --title .. --body .. --head <branch> [--base <b>]
		it := ghItem{Number: s.Next, State: "open", IsPR: true}
		it.Title = flagVal(args, "--title")
		it.Body = flagBodyVal(args, -1)
		it.HeadRef = flagVal(args, "--head")
		it.HeadRefOID = "headsha"
		it.Diff = "+e2e change\n"
		if s.CiBucket != "" {
			it.Checks = []ghCheck{{Bucket: s.CiBucket, Name: "e2e"}}
		}
		s.Next++
		s.Issues = append(s.Issues, it)
		s.save()
		fmt.Printf("https://github.com/o/r/pull/%d\n", it.Number)
	}
}

// ---- label ----
func handleLabel(args []string, s *store) {
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		writeJSON(s.Labels)
	case "create":
		if len(args) >= 2 {
			s.Labels = append(s.Labels, args[1])
			s.save()
		}
	}
}

// ---- api (reconcile.getIssue / reviews) ----
func handleAPI(args []string, s *store) {
	if len(args) < 1 {
		os.Exit(0)
	}
	if args[0] == "--method" {
		// gh api --method PATCH repos/o/r/issues/N -f state=X
		var method string
		for i := range args {
			if args[i] == "--method" && i+1 < len(args) {
				method = args[i+1]
			}
		}
		if method == "PATCH" {
			// find path argument
			for _, a := range args {
				var num int
				if n, err := fmt.Sscanf(a, "repos/o/r/issues/%d", &num); err == nil && n == 1 {
					idx := findIssue(s, num)
					if idx >= 0 {
						for j := 0; j < len(args); j++ {
							if args[j] == "-f" && j+1 < len(args) && strings.HasPrefix(args[j+1], "state=") {
								s.Issues[idx].State = strings.TrimPrefix(args[j+1], "state=")
							}
						}
						s.save()
					}
				}
			}
		}
		os.Exit(0)
	}
	// gh api repos/o/r/issues/N
	for _, a := range args {
		var num int
		if n, err := fmt.Sscanf(a, "repos/o/r/issues/%d", &num); err == nil && n == 1 {
			idx := findIssue(s, num)
			if idx < 0 {
				os.Exit(1)
			}
			it := s.Issues[idx]
			labels := make([]map[string]string, 0, len(it.Labels))
			for _, l := range it.Labels {
				labels = append(labels, map[string]string{"name": l})
			}
			writeJSON(map[string]interface{}{
				"number":       it.Number,
				"state":        it.State,
				"body":         it.Body,
				"labels":       labels,
				"pull_request": boolJSON(it.IsPR),
			})
			return
		}
		if n, err := fmt.Sscanf(a, "repos/o/r/issues/%d/comments", &num); err == nil && n == 1 {
			idx := findIssue(s, num)
			if idx < 0 {
				writeJSON([]map[string]string{})
				return
			}
			cmts := make([]map[string]string, 0, len(s.Issues[idx].Comments))
			for _, c := range s.Issues[idx].Comments {
				cmts = append(cmts, map[string]string{"body": c})
			}
			writeJSON(cmts)
			return
		}
	}
}

// ---- helpers ----
func boolJSON(b bool) interface{} {
	if b {
		return map[string]interface{}{}
	}
	return json.RawMessage("null")
}

func findIssue(s *store, n int) int {
	for i := range s.Issues {
		if s.Issues[i].Number == n {
			return i
		}
	}
	return -1
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func flagVal(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func flagBodyVal(args []string, i int) string {
	// body may contain spaces; find --body then take remainder
	for j := 0; j < len(args); j++ {
		if args[j] == "--body" && j+1 < len(args) {
			return strings.Join(args[j+1:], " ")
		}
	}
	return ""
}

func flagHas(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func remove(list []string, v string) []string {
	var out []string
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func writeJSON(v interface{}) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
