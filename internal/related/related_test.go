// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package related

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oscar/internal/diff"
	"golang.org/x/oscar/internal/docs"
	"golang.org/x/oscar/internal/embeddocs"
	"golang.org/x/oscar/internal/github"
	"golang.org/x/oscar/internal/githubdocs"
	"golang.org/x/oscar/internal/llm"
	"golang.org/x/oscar/internal/storage"
	"golang.org/x/oscar/internal/testutil"
)

var ctx = context.Background()

func TestRun(t *testing.T) {
	check := testutil.Checker(t)
	lg := testutil.Slogger(t)
	db := storage.MemDB()
	gh := github.New(lg, db, nil, nil)
	gh.Testing().LoadTxtar("../testdata/markdown.txt")
	gh.Testing().LoadTxtar("../testdata/rsctmp.txt")

	dc := docs.New(lg, db)
	githubdocs.Sync(ctx, lg, dc, gh)

	vdb := storage.MemVectorDB(db, lg, "vecs")
	embeddocs.Sync(ctx, lg, vdb, llm.QuoteEmbedder(), dc)

	vdb = storage.MemVectorDB(db, lg, "vecs")
	p := New(lg, db, gh, vdb, dc, "postname")
	p.EnableProject("rsc/markdown")
	p.SetTimeLimit(time.Time{})
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), nil)
	gh.Testing().ClearEdits()

	p.EnablePosts()
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), map[int64]string{13: post13, 19: post19})
	gh.Testing().ClearEdits()

	p = New(lg, db, gh, vdb, dc, "postname2")
	p.EnableProject("rsc/markdown")
	p.SetTimeLimit(time.Time{})
	p.EnablePosts()
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), nil)
	gh.Testing().ClearEdits()

	for i := range 4 {
		p := New(lg, db, gh, vdb, dc, "postnameloop."+fmt.Sprint(i))
		p.EnableProject("rsc/markdown")
		p.SetTimeLimit(time.Time{})
		switch i {
		case 0:
			p.SkipTitlePrefix("feature: ")
		case 1:
			p.SkipTitleSuffix("for heading")
		case 2:
			p.SkipBodyContains("For example, this heading")
		case 3:
			p.SkipBodyContains("For example, this heading")
			p.SkipBodyContains("ZZZ")
		}
		p.EnablePosts()
		p.deletePosted()
		check(p.Run(ctx))
		checkEdits(t, gh.Testing().Edits(), map[int64]string{13: post13})
		gh.Testing().ClearEdits()
	}

	p = New(lg, db, gh, vdb, dc, "postname3")
	p.EnableProject("rsc/markdown")
	p.SetMinScore(2.0) // impossible
	p.SetTimeLimit(time.Time{})
	p.EnablePosts()
	p.deletePosted()
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), nil)
	gh.Testing().ClearEdits()

	p = New(lg, db, gh, vdb, dc, "postname4")
	p.EnableProject("rsc/markdown")
	p.SetMinScore(2.0) // impossible
	p.SetTimeLimit(time.Date(2222, 1, 1, 1, 1, 1, 1, time.UTC))
	p.EnablePosts()
	p.deletePosted()
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), nil)
	gh.Testing().ClearEdits()

	p = New(lg, db, gh, vdb, dc, "postname5")
	p.EnableProject("rsc/markdown")
	p.SetMinScore(0)   // everything
	p.SetMaxResults(0) // except none
	p.SetTimeLimit(time.Time{})
	p.EnablePosts()
	p.deletePosted()
	check(p.Run(ctx))
	checkEdits(t, gh.Testing().Edits(), nil)
	gh.Testing().ClearEdits()
}

func TestPost(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		p, _, project, check := newTestPoster(t)

		check(p.Post(ctx, project, 19))
		check(p.Post(ctx, project, 13))
		checkEdits(t, p.github.Testing().Edits(), map[int64]string{13: post13, 19: post19})
	})

	t.Run("double-post", func(t *testing.T) {
		p, buf, project, check := newTestPoster(t)

		check(p.Post(ctx, project, 13))
		check(p.Post(ctx, project, 13))
		checkEdits(t, p.github.Testing().Edits(), map[int64]string{13: post13})

		testutil.ExpectLog(t, buf, "already posted", 1) // issue 13
	})

	t.Run("post-run", func(t *testing.T) {
		p, buf, project, check := newTestPoster(t)

		check(p.Post(ctx, project, 19))
		checkEdits(t, p.github.Testing().Edits(), map[int64]string{19: post19})
		testutil.ExpectLog(t, buf, "advanced watcher", 0)

		p.github.Testing().ClearEdits()

		// Post does not advance Run's watcher, so it operates on all issues.
		check(p.Run(ctx))
		checkEdits(t, p.github.Testing().Edits(), map[int64]string{13: post13})
		testutil.ExpectLog(t, buf, "already posted", 1)   // issue 19
		testutil.ExpectLog(t, buf, "advanced watcher", 2) // issue 13 and 19 both advance watcher

		p.github.Testing().ClearEdits()

		// Run is a no-op because previous call to run advanced watcher past issue 19.
		check(p.Run(ctx))
		checkEdits(t, p.github.Testing().Edits(), nil)
		testutil.ExpectLog(t, buf, "already posted", 1)   // no change
		testutil.ExpectLog(t, buf, "advanced watcher", 2) // no change
	})

	t.Run("post-run-async", func(t *testing.T) {
		p, buf, project, check := newTestPoster(t)

		// OK to run Post in the middle of a Run.
		done := make(chan struct{})
		go func() {
			check(p.Run(ctx))
			done <- struct{}{}
		}()
		check(p.Post(ctx, project, 19))
		<-done
		checkEdits(t, p.github.Testing().Edits(), map[int64]string{13: post13, 19: post19})

		testutil.ExpectLog(t, buf, "already posted", 1) // issue 19
	})
}

func TestPostError(t *testing.T) {
	t.Run("event not in DB", func(t *testing.T) {
		p, _, project, _ := newTestPoster(t)

		wantErr := errEventNotFound
		// issue 42 is not in the project
		if err := p.Post(ctx, project, 42); !errors.Is(err, wantErr) {
			t.Fatalf("Post err = %v, want %v", err, wantErr)
		}
	})

	t.Run("issue not in Vector DB", func(t *testing.T) {
		p, _, project, _ := newTestPoster(t)

		// Vector search will fail if there is no embedding
		// for the issue.
		id := int64(19)
		p.vdb.Delete(issueURL(project, id))

		wantErr := errVectorSearchFailed
		if err := p.Post(ctx, project, id); !errors.Is(err, wantErr) {
			t.Fatalf("Post err = %v, want %v", err, wantErr)
		}
	})
}

func newTestPoster(t *testing.T) (_ *Poster, out *bytes.Buffer, project string, check func(err error)) {
	t.Helper()

	lg, out := testutil.SlogBuffer()
	db := storage.MemDB()
	gh := github.New(lg, db, nil, nil)
	gh.Testing().LoadTxtar("../testdata/markdown.txt")
	gh.Testing().LoadTxtar("../testdata/rsctmp.txt")

	dc := docs.New(lg, db)
	githubdocs.Sync(ctx, lg, dc, gh)

	vdb := storage.MemVectorDB(db, lg, "vecs")
	embeddocs.Sync(ctx, lg, vdb, llm.QuoteEmbedder(), dc)

	t.Cleanup(gh.Testing().ClearEdits)
	p := New(lg, db, gh, vdb, dc, t.Name())
	project = "rsc/markdown"
	p.EnableProject(project)
	p.SetTimeLimit(time.Time{})
	p.EnablePosts()

	return p, out, project, testutil.Checker(t)
}

func checkEdits(t *testing.T, edits []*github.TestingEdit, want map[int64]string) {
	t.Helper()
	for _, e := range edits {
		if e.Project != "rsc/markdown" {
			t.Errorf("posted to unexpected project: %v", e)
			continue
		}
		if e.Comment != 0 || e.IssueCommentChanges == nil {
			t.Errorf("non-post edit: %v", e)
			continue
		}
		w, ok := want[e.Issue]
		if !ok {
			t.Errorf("post to unexpected issue: %v", e)
			continue
		}
		delete(want, e.Issue)
		if strings.TrimSpace(e.IssueCommentChanges.Body) != strings.TrimSpace(w) {
			t.Errorf("rsc/markdown#%d: wrong post:\n%s", e.Issue,
				string(diff.Diff("want", []byte(w), "have", []byte(e.IssueCommentChanges.Body))))
		}
	}
	for _, issue := range slices.Sorted(maps.Keys(want)) {
		t.Errorf("did not see post on rsc/markdown#%d", issue)
	}
	if t.Failed() {
		t.FailNow()
	}
}

var post13 = unQUOT(`**Related Issues and Documentation**

 - [goldmark and markdown diff with h1 inside p #6 (closed)](https://github.com/rsc/markdown/issues/6) <!-- score=0.92657 -->
 - [Support escaped \QUOT|\QUOT in table cells #9 (closed)](https://github.com/rsc/markdown/issues/9) <!-- score=0.91858 -->
 - [markdown: fix markdown printing for inline code #12 (closed)](https://github.com/rsc/markdown/issues/12) <!-- score=0.91325 -->
 - [markdown: emit Info in CodeBlock markdown #18 (closed)](https://github.com/rsc/markdown/issues/18) <!-- score=0.91129 -->
 - [feature: synthesize lowercase anchors for heading #19](https://github.com/rsc/markdown/issues/19) <!-- score=0.90867 -->
 - [Replace newlines with spaces in alt text #4 (closed)](https://github.com/rsc/markdown/issues/4) <!-- score=0.90859 -->
 - [allow capital X in task list items #2 (closed)](https://github.com/rsc/markdown/issues/2) <!-- score=0.90850 -->
 - [build(deps): bump golang.org/x/text from 0.3.6 to 0.3.8 in /rmplay #10](https://github.com/rsc/tmp/issues/10) <!-- score=0.90453 -->
 - [Render reference links in Markdown #14 (closed)](https://github.com/rsc/markdown/issues/14) <!-- score=0.90175 -->
 - [Render reference links in Markdown #15 (closed)](https://github.com/rsc/markdown/issues/15) <!-- score=0.90103 -->

<sub>(Emoji vote if this was helpful or unhelpful; more detailed feedback welcome in [this discussion](https://github.com/golang/go/discussions/67901).)</sub>
`)

var post19 = unQUOT(`**Related Issues and Documentation**

 - [allow capital X in task list items #2 (closed)](https://github.com/rsc/markdown/issues/2) <!-- score=0.92943 -->
 - [Support escaped \QUOT|\QUOT in table cells #9 (closed)](https://github.com/rsc/markdown/issues/9) <!-- score=0.91994 -->
 - [goldmark and markdown diff with h1 inside p #6 (closed)](https://github.com/rsc/markdown/issues/6) <!-- score=0.91813 -->
 - [Render reference links in Markdown #14 (closed)](https://github.com/rsc/markdown/issues/14) <!-- score=0.91513 -->
 - [Render reference links in Markdown #15 (closed)](https://github.com/rsc/markdown/issues/15) <!-- score=0.91487 -->
 - [Empty column heading not recognized in table #7 (closed)](https://github.com/rsc/markdown/issues/7) <!-- score=0.90874 -->
 - [Correctly render reference links in Markdown #13](https://github.com/rsc/markdown/issues/13) <!-- score=0.90867 -->
 - [markdown: fix markdown printing for inline code #12 (closed)](https://github.com/rsc/markdown/issues/12) <!-- score=0.90795 -->
 - [Replace newlines with spaces in alt text #4 (closed)](https://github.com/rsc/markdown/issues/4) <!-- score=0.90278 -->
 - [build(deps): bump golang.org/x/text from 0.3.6 to 0.3.8 in /rmplay #10](https://github.com/rsc/tmp/issues/10) <!-- score=0.90259 -->

<sub>(Emoji vote if this was helpful or unhelpful; more detailed feedback welcome in [this discussion](https://github.com/golang/go/discussions/67901).)</sub>
`)

func unQUOT(s string) string { return strings.ReplaceAll(s, "QUOT", "`") }
