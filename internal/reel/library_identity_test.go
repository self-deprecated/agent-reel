package reel

import "testing"

func TestLibraryPaletteMatchesProjectFooter(t *testing.T) {
	// Golden values obtained by evaluating the actual TypeScript footer functions,
	// not by repeating this Go implementation. Tests themselves need only Go.
	for _, tc := range []struct{ path, bg, fg string }{
		{"/home/jmo/Development/mono/agent", "#c0af16", "#ffffff"},
		{"/home/jmo/Development/workspaces/agent/areel-ux", "#1a27b7", "#ffffff"},
		// A workspace segment: the footer hashes the workspace path itself
		// (keyedPalette(resolve(workspace.path))); "overview" is red there.
		{"/home/jmo/Development/workspaces/agent/overview", "#b51215", "#ffffff"},
		{"/tmp/🎥/é", "#41b620", "#ffffff"},
		{"/", "#b33d19", "#ffffff"},
		{"/tmp/same", "#6f21ab", "#ffffff"},
		{"/another/same", "#14d2b9", "#000000"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := libraryPathPart(tc.path, "label")
			if got.Background != tc.bg || got.Foreground != tc.fg || got.Path != tc.path || got.Label != "label" {
				t.Fatalf("palette %+v", got)
			}
		})
	}
}

func TestLibraryIdentityUsesPersistedPathsOnly(t *testing.T) {
	// None of these paths needs to exist. No ambient cwd, JJ state or HOME lookup.
	t.Setenv("HOME", "/unrelated")
	for _, tc := range []struct{ name, cwd, main, repo, repoPath, workspace, workspacePath string }{
		{"workspace", "/home/jmo/Development/workspaces/agent/areel-ux/internal", "/home/jmo/Development/mono/agent", "mono/agent", "/home/jmo/Development/mono/agent", "areel-ux", "/home/jmo/Development/workspaces/agent/areel-ux"},
		{"missing main", "/missing/workspaces/project/task/subdir", "", "project", "/missing/workspaces/project", "task", "/missing/workspaces/project/task"},
		{"ordinary", "/home/u/Development/mono/project", "", "mono/project", "/home/u/Development/mono/project", "", ""},
		{"outside Development", "/srv/project", "", "project", "/srv/project", "", ""},
		{"root", "/", "", "/", "/", "", ""},
		{"main only", "", "/home/u/Development/mono/repo", "mono/repo", "/home/u/Development/mono/repo", "", ""},
		{"invalid", "relative", "/a/../b", "Unknown repository", "", "", ""},
		{"last Development", "/Development/old/Development/new", "", "new", "/Development/old/Development/new", "", ""},
		{"trailing Development", "/Development/old/Development", "", "Development", "/Development/old/Development", "", ""},
		{"trailing workspaces", "/workspaces/a/b/workspaces", "", "workspaces", "/workspaces/a/b/workspaces", "", ""},
		{"hostile text", "/Development/<script>/🎥", "", "<script>/🎥", "/Development/<script>/🎥", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := libraryRecordingIdentity(&libraryContext{WorkingDirectory: tc.cwd, JJMain: tc.main})
			if got.Repository.Label != tc.repo || got.Repository.Path != tc.repoPath {
				t.Fatalf("repository %+v", got)
			}
			if tc.workspace == "" {
				if got.Workspace != nil {
					t.Fatalf("invented workspace %+v", got.Workspace)
				}
				return
			}
			if got.Workspace == nil || got.Workspace.Label != tc.workspace || got.Workspace.Path != tc.workspacePath {
				t.Fatalf("workspace %+v", got.Workspace)
			}
		})
	}
	if libraryRecordingIdentity(nil).Repository.Label != "Unknown repository" {
		t.Fatal("missing context")
	}
}

// The workspace chip uses the footer palette of the workspace path, the same
// key pi/project-footer hashes for its workspace segment.
func TestLibraryIdentityWorkspaceMatchesFooter(t *testing.T) {
	got := libraryRecordingIdentity(&libraryContext{WorkingDirectory: "/home/jmo/Development/workspaces/agent/overview/tools/agent-reel", JJMain: "/home/jmo/Development/mono/agent"})
	if got.Workspace == nil || got.Workspace.Label != "overview" || got.Workspace.Background != "#b51215" || got.Workspace.Foreground != "#ffffff" {
		t.Fatalf("workspace %+v", got.Workspace)
	}
	if got.Repository.Background != "#c0af16" {
		t.Fatalf("repository %+v", got.Repository)
	}
	want := libraryPathPart("/home/jmo/Development/workspaces/agent/overview", "overview")
	if got.Workspace.Tint != want.Tint || got.Workspace.Ink != want.Ink || got.Workspace.TintDark != want.TintDark || got.Workspace.InkDark != want.InkDark || got.Workspace.Tint == got.Repository.Tint {
		t.Fatalf("workspace chip colours %+v", got.Workspace)
	}
}
