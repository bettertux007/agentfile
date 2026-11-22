package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

func TestRunFastPathHitAndInvalidations(t *testing.T) {
	t.Parallel()
	proj := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	extDir := filepath.Join(cacheDir, "extracted", "abc")
	localSkill := filepath.Join(proj, "skills", "pay")
	outSkill := filepath.Join(proj, ".claude", "skills", "pay")

	_ = os.MkdirAll(extDir, 0o755)
	_ = os.MkdirAll(filepath.Join(localSkill, "references"), 0o755)
	_ = os.MkdirAll(filepath.Join(outSkill, "references"), 0o755)

	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude, opencode]\n"), 0o644)
	_ = os.WriteFile(filepath.Join(localSkill, "SKILL.md"), []byte("# Pay\n"), 0o644)
	_ = os.WriteFile(filepath.Join(localSkill, "references", "guide.md"), []byte("v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(outSkill, "SKILL.md"), []byte("# Pay\n"), 0o644)
	_ = os.WriteFile(filepath.Join(outSkill, "references", "guide.md"), []byte("v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte("{}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("# Shared\n"), 0o644)

	st := &State{
		Version: StateVersion,
		Targets: map[string]*TargetState{
			"claude": {
				Artifacts: map[string]ArtifactRecord{
					".claude/skills/pay": {ArtifactType: TypeDir, SHA256: "sha256:1"},
					".mcp.json":          {ArtifactType: TypeJSONCPatch, SHA256: "sha256:2"},
				},
			},
		},
		Shared: map[string]SharedArtifactRecord{
			"AGENTS.md": {
				Record: ArtifactRecord{ArtifactType: TypeMarkdownBlock, SHA256: "sha256:3"},
				Owners: []string{"claude"},
			},
		},
	}
	if err := SaveState(proj, st); err != nil {
		t.Fatal(err)
	}

	effCfg := &config.EffectiveConfig{
		LinkMode:        "copy",
		OnConflict:      "fail",
		GitExclude:      false,
		LocalConfigPath: filepath.Join(proj, ".agentfile", "config.yaml"),
	}
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude", "opencode"},
		MCP: map[string]spec.MCPServerSpec{
			"gh": {Transport: "http", URL: spec.EnvString{Raw: "https://x"}},
		},
	}
	probes := adapters.ProbeResults{"agents_md": true}
	resolved := map[string]store.ResolvedAsset{
		"skill:pay": {Kind: "skill", Name: "pay", IsRemote: false, SourcePath: localSkill},
		"skill:rem": {Kind: "skill", Name: "rem", IsRemote: true, SourcePath: extDir},
	}

	saveIn := RunFastPathSaveInput{
		ProjectRoot:      proj,
		AgentfileVersion: "0.1.0",
		Target:           "claude",
		EffectiveSpec:    ps,
		EffectiveConfig:  effCfg,
		CacheRoot:        cacheDir,
		Probes:           probes,
		ResolvedAssets:   resolved,
	}
	if err := SaveRunFastPath(saveIn); err != nil {
		t.Fatalf("SaveRunFastPath failed: %v", err)
	}

	checkIn := RunFastPathCheckInput{
		ProjectRoot:      proj,
		AgentfileVersion: "0.1.0",
		Target:           "claude",
		EffectiveConfig:  effCfg,
		CacheRoot:        cacheDir,
		Probes:           probes,
	}

	// 1. Immediate warm check hits fast path (verifying racy candidates via SHA-256).
	rec, hit, reason := CheckRunFastPath(checkIn)
	if !hit || rec == nil {
		t.Fatalf("expected immediate fast-path hit, got miss: %s", reason)
	}
	bundle := rec.BuildBundleForRun()
	if len(bundle.MCP) != 1 || bundle.MCP[0].Name != "gh" {
		t.Fatalf("unexpected cached bundle: %+v", bundle)
	}

	// 2. Different target (opencode) misses cleanly (per-target isolation).
	checkOpencode := checkIn
	checkOpencode.Target = "opencode"
	if _, hit, _ := CheckRunFastPath(checkOpencode); hit {
		t.Fatalf("expected fast-path miss for unsaved target opencode")
	}

	// 3. Same-timestamp, same-size content edit on a racy file is caught by SHA-256 check!
	guideFile := filepath.Join(localSkill, "references", "guide.md")
	origInfo, _ := os.Lstat(guideFile)
	_ = os.WriteFile(guideFile, []byte("v2\n"), 0o644)
	_ = os.Chtimes(guideFile, origInfo.ModTime(), origInfo.ModTime())
	if _, hit, reason := CheckRunFastPath(checkIn); hit {
		t.Fatalf("expected racy content edit with identical mtime/size to miss fast path")
	} else if reason == "" {
		t.Fatalf("expected non-empty miss reason")
	}
	// Restore v1 and original mtime -> hits again!
	_ = os.WriteFile(guideFile, []byte("v1\n"), 0o644)
	_ = os.Chtimes(guideFile, origInfo.ModTime(), origInfo.ModTime())
	if _, hit, reason := CheckRunFastPath(checkIn); !hit {
		t.Fatalf("expected hit after restoring v1, got miss: %s", reason)
	}

	// 4. chmod (read-only mode change, portable across POSIX and Windows) on local skill file invalidates fast path.
	_ = os.Chmod(guideFile, 0o444)
	if _, hit, _ := CheckRunFastPath(checkIn); hit {
		t.Fatalf("expected chmod 0444 to invalidate fast path")
	}
	_ = os.Chmod(guideFile, 0o644)
	_ = os.Chtimes(guideFile, origInfo.ModTime(), origInfo.ModTime())

	// 5. Creating a new file inside local skill directory invalidates fast path.
	extraFile := filepath.Join(localSkill, "references", "new.md")
	_ = os.WriteFile(extraFile, []byte("new\n"), 0o644)
	if _, hit, _ := CheckRunFastPath(checkIn); hit {
		t.Fatalf("expected new file in local skill directory to invalidate fast path")
	}
	_ = os.Remove(extraFile)

	// 6. Editing a nested file inside TypeDir output (.claude/skills/pay/references/guide.md) invalidates fast path.
	outGuide := filepath.Join(outSkill, "references", "guide.md")
	outOrigInfo, _ := os.Lstat(outGuide)
	_ = os.WriteFile(outGuide, []byte("tampered\n"), 0o644)
	if _, hit, _ := CheckRunFastPath(checkIn); hit {
		t.Fatalf("expected nested TypeDir output edit to invalidate fast path")
	}
	_ = os.WriteFile(outGuide, []byte("v1\n"), 0o644)
	_ = os.Chtimes(outGuide, outOrigInfo.ModTime(), outOrigInfo.ModTime())

	// 7. Probe change (e.g. AGENTS.md probe flipping) invalidates fast path.
	badProbes := checkIn
	badProbes.Probes = adapters.ProbeResults{"agents_md": false}
	if _, hit, _ := CheckRunFastPath(badProbes); hit {
		t.Fatalf("expected probe change to invalidate fast path")
	}

	// 8. CacheRoot change invalidates fast path.
	badCache := checkIn
	badCache.CacheRoot = "/different/cache"
	if _, hit, _ := CheckRunFastPath(badCache); hit {
		t.Fatalf("expected CacheRoot change to invalidate fast path")
	}

	// 9. Pending commit-journal.json invalidates fast path.
	_ = os.WriteFile(JournalPath(proj), []byte("{}\n"), 0o644)
	if _, hit, reason := CheckRunFastPath(checkIn); hit || reason != "commit journal pending" {
		t.Fatalf("expected commit journal pending miss, got hit=%v reason=%q", hit, reason)
	}
	_ = os.Remove(JournalPath(proj))

	// 10. Clean removes .agentfile/run-cache/claude.json.
	if _, err := Clean(proj, nil, false); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	if _, err := os.Stat(RunCachePath(proj, "claude")); !os.IsNotExist(err) {
		t.Fatalf("expected Clean to remove .agentfile/run-cache/claude.json")
	}

	// 11. Test symlink output, GitExclude=true, UserLoadGlobal/LocalLoadGlobal env digest, and timestamp promotion.
	symTarget := filepath.Join(proj, "skills", "pay")
	symOut := filepath.Join(proj, ".claude", "skills", "sym")
	_ = os.MkdirAll(filepath.Dir(symOut), 0o755)
	_ = os.Symlink(symTarget, symOut)
	_ = os.MkdirAll(filepath.Join(proj, ".git", "info"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".git", "info", "exclude"), []byte("# exclude\n"), 0o644)

	stSym := &State{
		Version: StateVersion,
		Targets: map[string]*TargetState{
			"claude": {
				Artifacts: map[string]ArtifactRecord{
					".claude/skills/sym": {ArtifactType: TypeSymlink},
				},
			},
		},
	}
	_ = SaveState(proj, stSym)
	bTrue := true
	bFalse := false
	effSym := &config.EffectiveConfig{
		LinkMode:        "symlink",
		OnConflict:      "fail",
		GitExclude:      true,
		UserLoadGlobal:  &bTrue,
		LocalLoadGlobal: &bFalse,
		LocalConfigPath: filepath.Join(proj, ".agentfile", "config.yaml"),
		UserConfigPath:  filepath.Join(proj, ".agentfile", "user-config.yaml"),
	}
	_ = SaveRunFastPath(RunFastPathSaveInput{
		ProjectRoot:        proj,
		AgentfileVersion:   "0.1.0",
		Target:             "claude",
		EffectiveSpec:      ps,
		LocalManifests:     []string{filepath.Join(proj, "Agentfile.yaml")},
		EffectiveConfig:    effSym,
		CacheRoot:          cacheDir,
		ResolvedAssets:     resolved,
		ExtraExtractedDirs: []string{extDir},
	})
	if _, hit, reason := CheckRunFastPath(RunFastPathCheckInput{
		ProjectRoot:      proj,
		AgentfileVersion: "0.1.0",
		Target:           "claude",
		EffectiveConfig:  effSym,
		CacheRoot:        cacheDir,
	}); !hit {
		t.Fatalf("expected symlink + git_exclude fast-path hit, got miss: %s", reason)
	}
}

func BenchmarkRunFastPath(b *testing.B) {
	for _, count := range []int{100, 1000, 2000} {
		b.Run(fmt.Sprintf("files_%d", count), func(b *testing.B) {
			proj := b.TempDir()
			cacheDir := filepath.Join(b.TempDir(), "cache")
			localSkill := filepath.Join(proj, "skills", "bench")
			outFile := filepath.Join(proj, ".mcp.json")
			_ = os.MkdirAll(localSkill, 0o755)
			_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)
			_ = os.WriteFile(outFile, []byte("{}\n"), 0o644)

			oldTime := time.Now().Add(-10 * time.Second)
			// Create count-2 regular files inside localSkill (plus localSkill dir + outFile == count total entries).
			for i := 0; i < count-2; i++ {
				p := filepath.Join(localSkill, fmt.Sprintf("f_%04d.md", i))
				_ = os.WriteFile(p, []byte("x\n"), 0o644)
				_ = os.Chtimes(p, oldTime, oldTime)
			}
			_ = os.Chtimes(localSkill, oldTime, oldTime)
			_ = os.Chtimes(outFile, oldTime, oldTime)

			st := &State{
				Version: StateVersion,
				Targets: map[string]*TargetState{
					"claude": {
						Artifacts: map[string]ArtifactRecord{
							".mcp.json": {ArtifactType: TypeJSONCPatch, SHA256: "sha256:1"},
						},
					},
				},
			}
			_ = SaveState(proj, st)

			effCfg := &config.EffectiveConfig{
				LinkMode:        "copy",
				OnConflict:      "fail",
				LocalConfigPath: filepath.Join(proj, ".agentfile", "config.yaml"),
			}
			ps := &spec.ProjectSpec{Version: 1, Targets: []string{"claude"}}
			_ = SaveRunFastPath(RunFastPathSaveInput{
				ProjectRoot:      proj,
				AgentfileVersion: "0.1.0",
				Target:           "claude",
				EffectiveSpec:    ps,
				EffectiveConfig:  effCfg,
				CacheRoot:        cacheDir,
				ResolvedAssets: map[string]store.ResolvedAsset{
					"skill:bench": {Kind: "skill", Name: "bench", SourcePath: localSkill},
				},
			})

			checkIn := RunFastPathCheckInput{
				ProjectRoot:      proj,
				AgentfileVersion: "0.1.0",
				Target:           "claude",
				EffectiveConfig:  effCfg,
				CacheRoot:        cacheDir,
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, hit, reason := CheckRunFastPath(checkIn); !hit {
					b.Fatalf("expected hit, got miss: %s", reason)
				}
			}
		})
	}
}
