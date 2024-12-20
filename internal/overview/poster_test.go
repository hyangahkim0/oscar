// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package overview

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/oscar/internal/actions"
	"golang.org/x/oscar/internal/github"
	"golang.org/x/oscar/internal/llmapp"
	"golang.org/x/oscar/internal/storage"
	"golang.org/x/oscar/internal/testutil"
)

var (
	jan1_2024  = "2024-01-01T00:00:00Z"
	jan1_2023  = "2023-01-01T00:00:00Z"
	dec31_2023 = "2023-12-31T00:00:00Z"
	dec30_2023 = "2023-12-30T00:00:00Z"
	dec31_2022 = "2022-12-31T00:00:00Z"
)

func TestRun(t *testing.T) {
	lg := testutil.Slogger(t)
	project := "test/test"
	check := testutil.Checker(t)
	ctx := context.Background()
	// "now" is Jan 1, 2024.
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Set default minimum comments to 1 for testing.
	old := defaultMinComments
	defaultMinComments = 1
	t.Cleanup(func() { defaultMinComments = old })

	// Add two issues created "now".
	// Issue 1 has two comments.
	// Issue 2 has 1 comment.
	basicSetup := func(gh *github.Client) {
		gh.Testing().AddIssue(project, &github.Issue{Number: 1, Body: "issue 1", CreatedAt: jan1_2024})
		gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 1"})
		gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 2"})

		gh.Testing().AddIssue(project, &github.Issue{Number: 2, Body: "issue 2", CreatedAt: jan1_2024})
		gh.Testing().AddIssueComment(project, 2, &github.IssueComment{Body: "issue 2 comment 1"})
	}

	for _, tc := range []struct {
		name        string
		setup       func(gh *github.Client) // function to add test data
		minComments *int
		maxAge      *time.Duration
		autoApprove *bool
		wantReport  *actions.RunReport
		wantEdits   []*github.TestingEdit
	}{
		{
			name:  "basic",
			setup: basicSetup,
			wantReport: &actions.RunReport{
				Completed: 2,
			},
			autoApprove: ptr(true),
			wantEdits: []*github.TestingEdit{
				{Project: project, Issue: 1, IssueCommentChanges: &github.IssueCommentChanges{
					Body: comment("an overview of issue 1 with 2 comment(s)"),
				}},
				{Project: project, Issue: 1, IssueChanges: &github.IssueChanges{
					Body: "issue 1\n@testbot's overview of this issue: test-url",
				}},
				{Project: project, Issue: 2, IssueCommentChanges: &github.IssueCommentChanges{
					Body: comment("an overview of issue 2 with 1 comment(s)"),
				}},
				{Project: project, Issue: 2, IssueChanges: &github.IssueChanges{
					Body: "issue 2\n@testbot's overview of this issue: test-url",
				}},
			},
		},
		{
			name: "max age default",
			setup: func(gh *github.Client) {
				// Exactly meets cutoff (1 year)
				gh.Testing().AddIssue(project, &github.Issue{Number: 1, Body: "issue 1", CreatedAt: jan1_2023})
				gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 1"})

				// Too old.
				gh.Testing().AddIssue(project, &github.Issue{Number: 2, Body: "issue 2", CreatedAt: dec31_2022})
				gh.Testing().AddIssueComment(project, 2, &github.IssueComment{Body: "issue 2 comment 1"})
			},
			autoApprove: ptr(true),
			wantReport: &actions.RunReport{
				Completed: 1,
			},
			wantEdits: []*github.TestingEdit{
				{Project: project, Issue: 1, IssueCommentChanges: &github.IssueCommentChanges{
					Body: comment("an overview of issue 1 with 1 comment(s)"),
				}},
				{Project: project, Issue: 1, IssueChanges: &github.IssueChanges{
					Body: "issue 1\n@testbot's overview of this issue: test-url",
				}},
			},
		},
		{
			name: "max age set",
			setup: func(gh *github.Client) {
				// Exactly meets cutoff.
				gh.Testing().AddIssue(project, &github.Issue{Number: 1, Body: "issue 1", CreatedAt: dec31_2023})
				gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 1"})

				// Too old.
				gh.Testing().AddIssue(project, &github.Issue{Number: 2, Body: "issue 2", CreatedAt: dec30_2023})
				gh.Testing().AddIssueComment(project, 2, &github.IssueComment{Body: "issue 2 comment 1"})
			},
			autoApprove: ptr(true),
			maxAge:      ptr(time.Hour * 24),
			wantReport: &actions.RunReport{
				Completed: 1,
			},
			wantEdits: []*github.TestingEdit{
				{Project: project, Issue: 1, IssueCommentChanges: &github.IssueCommentChanges{
					Body: comment("an overview of issue 1 with 1 comment(s)"),
				}},
				{Project: project, Issue: 1, IssueChanges: &github.IssueChanges{
					Body: "issue 1\n@testbot's overview of this issue: test-url",
				}},
			},
		},
		// TODO(tatianabradley): Additional unit test cases:
		//  - Other configuration (min comments, project, auto-approve)
		//  - Ignored events
		//  - Skipped issues
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := storage.MemDB()
			gh := github.New(lg, db, nil, nil)
			tc.setup(gh)

			p := newPoster(lg, db, gh, "test", "testbot")
			p.EnableProject(project)
			if tc.minComments != nil {
				p.SetMinComments(*tc.minComments)
			}
			if tc.maxAge != nil {
				p.SetMaxIssueAge(*tc.maxAge)
			}
			if tc.autoApprove != nil {
				if *tc.autoApprove {
					p.AutoApprove()
				} else {
					p.RequireApproval()
				}
			}

			check(p.run(ctx, overviewFuncForTest(gh), now))
			gotReport := actions.RunWithReport(ctx, lg, db)
			if diff := cmp.Diff(tc.wantReport, gotReport); diff != "" {
				t.Errorf("actions.RunWithReport mismatch (-want +got)\n:%s", diff)
			}
			if diff := cmp.Diff(tc.wantEdits, gh.Testing().Edits()); diff != "" {
				t.Errorf("edits mismatch (-want +got)\n:%s", diff)
			}
		})
	}
}

func ptr[T any](v T) *T {
	return &v
}

func TestRunUpdate(t *testing.T) {
	lg := testutil.Slogger(t)
	db := storage.MemDB()
	gh := github.New(lg, db, nil, nil)
	project := "test/test"
	check := testutil.Checker(t)
	ctx := context.Background()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	gh.Testing().AddIssue(project, &github.Issue{Number: 1, Body: "issue 1", CreatedAt: jan1_2024})
	gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 1"})
	gh.Testing().AddIssueComment(project, 1, &github.IssueComment{Body: "issue 1 comment 2"})

	gh.Testing().AddIssue(project, &github.Issue{Number: 2, Body: "issue 2", CreatedAt: jan1_2024})
	gh.Testing().AddIssueComment(project, 2, &github.IssueComment{Body: "issue 2 comment 1"})

	p := newPoster(lg, db, gh, "test", "testbot")
	p.EnableProject(project)
	p.SetMinComments(1)
	p.AutoApprove()

	// Use a test implementation to run the post action, which adds the expected
	// values to the github testing client instead of diverting edits.
	p.logAction = actions.Register(actionKind, &testPoster{p: p})
	check(p.run(ctx, overviewFuncForTest(gh), now))
	check(actions.Run(ctx, lg, db))

	// Run again to pick up the new comments (added by the previous run).
	// (Note that comments by the bot are ignored when generating the real
	// overview).
	check(p.run(ctx, overviewFuncForTest(gh), now))
	check(actions.Run(ctx, lg, db))
	wantEdits := []*github.TestingEdit{
		{Project: project, Issue: 1, Comment: 10000000004,
			IssueCommentChanges: &github.IssueCommentChanges{
				Body: comment("an overview of issue 1 with 3 comment(s)"),
			}},
		{Project: project, Issue: 2, Comment: 10000000005, IssueCommentChanges: &github.IssueCommentChanges{
			Body: comment("an overview of issue 2 with 2 comment(s)"),
		}},
	}
	if diff := cmp.Diff(wantEdits, gh.Testing().Edits()); diff != "" {
		t.Errorf("run update: edits mismatch (-want +got)\n:%s", diff)
	}
}

// testPoster is a test implementation of [actioner]
// that, for post actions, modifies the GitHub testing database (instead
// of diverting edits, which is what happens when we use
// the real [actioner] implementation).
// it uses the real implementation for update actions.
type testPoster struct {
	p *poster
}

func (p *testPoster) Run(ctx context.Context, data []byte) ([]byte, error) {
	return runFromActionLog(ctx, data, p.runTestAction)
}

func (*testPoster) ForDisplay([]byte) string { return "" }

func (tp *testPoster) runTestAction(ctx context.Context, a *action) (*result, error) {
	// Use test implementation for post actions.
	if a.isPost() {
		n := tp.p.gh.Testing().AddIssueComment(a.Issue.Project(), a.Issue.Number, &github.IssueComment{
			Body: a.Changes.Body,
		})
		url := fmt.Sprintf("%s#issuecomment-%d", a.Issue.HTMLURL, n)
		tp.p.gh.Testing().AddIssue(a.Issue.Project(),
			&github.Issue{Number: a.Issue.Number, Body: appendCommentURL(a.Issue, tp.p.bot, url), CreatedAt: a.Issue.CreatedAt})
		return &result{URL: url}, nil
	}
	return tp.p.runUpdateAction(ctx, a)
}

// overviewFuncForTest returns an overviewFunc that returns a phrase based on
// the issue number and its number of comments.
func overviewFuncForTest(gh *github.Client) overviewFunc {
	return func(ctx context.Context, i *github.Issue) (*IssueResult, error) {
		cs := slices.Collect(gh.Comments(i))
		return &IssueResult{
			TotalComments: len(cs),
			LastComment:   cs[len(cs)-1].CommentID(),
			Overview: &llmapp.Result{
				Response: fmt.Sprintf("an overview of issue %d with %d comment(s)", i.Number, len(cs)),
			},
		}, nil
	}
}
