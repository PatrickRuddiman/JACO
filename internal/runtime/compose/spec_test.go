package compose_test

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// volumeBody is the smallest compose snippet for the volume-scoping tests:
// one service mounts a single named volume `data` at `/data`. Tests vary the
// top-level `volumes:` block to exercise the override matrix without
// reparsing per case.
const volumeBody = `services:
  app:
    image: nginx
    volumes:
      - data:/data
volumes:
  data: {}
`

// loadAppSpec is the per-test boilerplate: parse the given body, project the
// `app` service with the given deployment and overrides, return its Mounts.
// Tests assert against Mounts (or full spec when broader fields matter)
// rather than reaching into the project mid-test.
func loadAppSpec(t *testing.T, body, deployment string, overrides map[string]string) []compose.Mount {
	t.Helper()
	project, err := compose.LoadBytes([]byte(body), "memory.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	svc, ok := lookupService(project, "app")
	if !ok {
		t.Fatal("app service missing from project")
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		Deployment:          deployment,
		Service:             "app",
		VolumeNameOverrides: overrides,
	})
	return spec.Mounts
}

// TestToContainerSpec_VolumeUsesDeploymentPrefix — a bare top-level
// `data: {}` declaration scopes the resulting docker volume to
// `jaco_<deployment>_data`, isolating it from any other deployment that
// happens to use the same volume key.
func TestToContainerSpec_VolumeUsesDeploymentPrefix(t *testing.T) {
	mounts := loadAppSpec(t, volumeBody, "stack-a", nil)
	if len(mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1; got %+v", len(mounts), mounts)
	}
	got := mounts[0]
	if got.Type != types.VolumeTypeVolume {
		t.Errorf("Type = %q, want %q", got.Type, types.VolumeTypeVolume)
	}
	if want := "jaco_stack-a_data"; got.Source != want {
		t.Errorf("Source = %q, want %q", got.Source, want)
	}
	if got.Target != "/data" {
		t.Errorf("Target = %q, want %q", got.Target, "/data")
	}
}

// TestToContainerSpec_VolumeHonorsTopLevelNameOverride — when the
// operator sets `volumes.<key>.name: <literal>`, the literal is used
// unprefixed. This is the compose-portable escape hatch for sharing
// storage across stacks.
func TestToContainerSpec_VolumeHonorsTopLevelNameOverride(t *testing.T) {
	body := `services:
  app:
    image: nginx
    volumes:
      - data:/data
volumes:
  data:
    name: ops-shared
`
	overrides, err := compose.TopLevelVolumeNames([]byte(body))
	if err != nil {
		t.Fatalf("TopLevelVolumeNames: %v", err)
	}
	if got, want := overrides["data"], "ops-shared"; got != want {
		t.Fatalf("override[data] = %q, want %q", got, want)
	}
	mounts := loadAppSpec(t, body, "stack-a", overrides)
	if len(mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(mounts))
	}
	if got, want := mounts[0].Source, "ops-shared"; got != want {
		t.Errorf("Source = %q, want %q (unprefixed)", got, want)
	}
}

// TestToContainerSpec_BindMountUnchanged — bind mounts (Type "bind",
// Source is a host path) are never rewritten. The deployment prefix
// only applies to named volumes.
func TestToContainerSpec_BindMountUnchanged(t *testing.T) {
	body := `services:
  app:
    image: nginx
    volumes:
      - /etc/nginx/conf.d:/etc/nginx/conf.d:ro
`
	mounts := loadAppSpec(t, body, "stack-a", nil)
	if len(mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(mounts))
	}
	got := mounts[0]
	if got.Type != types.VolumeTypeBind {
		t.Errorf("Type = %q, want %q", got.Type, types.VolumeTypeBind)
	}
	if want := "/etc/nginx/conf.d"; got.Source != want {
		t.Errorf("Source = %q, want %q (bind path unchanged)", got.Source, want)
	}
	if !got.ReadOnly {
		t.Errorf("ReadOnly = false, want true")
	}
}

// TestToContainerSpec_AnonymousVolumeUnchanged — anonymous volumes
// (compose `volumes: [/data]` — only a target, no source) become Type
// "volume" with Source "" after compose-go's normalisation. The prefix
// helper must leave the empty source alone so docker generates a name.
func TestToContainerSpec_AnonymousVolumeUnchanged(t *testing.T) {
	body := `services:
  app:
    image: nginx
    volumes:
      - /data
`
	mounts := loadAppSpec(t, body, "stack-a", nil)
	if len(mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(mounts))
	}
	got := mounts[0]
	if got.Type != types.VolumeTypeVolume {
		t.Errorf("Type = %q, want %q", got.Type, types.VolumeTypeVolume)
	}
	if got.Source != "" {
		t.Errorf("Source = %q, want empty (anonymous)", got.Source)
	}
	if got.Target != "/data" {
		t.Errorf("Target = %q, want %q", got.Target, "/data")
	}
}

// TestToContainerSpec_TwoDeploymentsSameKeyDistinctVolume — the same
// compose document projected with two different deployment names yields
// distinct Source strings. This is the regression guard: the bug this
// change fixes was that both projections produced the bare key `data`
// and the two deployments silently shared backing storage.
func TestToContainerSpec_TwoDeploymentsSameKeyDistinctVolume(t *testing.T) {
	frontMounts := loadAppSpec(t, volumeBody, "vol-front", nil)
	backMounts := loadAppSpec(t, volumeBody, "vol-back", nil)
	if len(frontMounts) != 1 || len(backMounts) != 1 {
		t.Fatalf("Mounts len: front=%d back=%d", len(frontMounts), len(backMounts))
	}
	front := frontMounts[0].Source
	back := backMounts[0].Source
	if front == back {
		t.Fatalf("two deployments collide on Source %q (same backing volume)", front)
	}
	if want := "jaco_vol-front_data"; front != want {
		t.Errorf("front Source = %q, want %q", front, want)
	}
	if want := "jaco_vol-back_data"; back != want {
		t.Errorf("back Source = %q, want %q", back, want)
	}
}

// TestTopLevelVolumeNames_OnlyExplicitOverrides — bare `data: {}`
// produces no override (default scoping wins); explicit `name:` produces
// an override; explicit `external: true` (with or without `name:`)
// produces an override. Empty / missing top-level block yields nil.
func TestTopLevelVolumeNames_OnlyExplicitOverrides(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "no_top_level_block",
			body: "services:\n  app:\n    image: nginx\n",
			want: nil,
		},
		{
			name: "bare_body_no_override",
			body: "services:\n  app:\n    image: nginx\nvolumes:\n  data: {}\n",
			want: nil,
		},
		{
			name: "explicit_name",
			body: "services:\n  app:\n    image: nginx\nvolumes:\n  data:\n    name: ops-shared\n",
			want: map[string]string{"data": "ops-shared"},
		},
		{
			name: "external_no_name_uses_key",
			body: "services:\n  app:\n    image: nginx\nvolumes:\n  ext:\n    external: true\n",
			want: map[string]string{"ext": "ext"},
		},
		{
			name: "external_with_explicit_name",
			body: "services:\n  app:\n    image: nginx\nvolumes:\n  ext:\n    external: true\n    name: legacy-name\n",
			want: map[string]string{"ext": "legacy-name"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compose.TopLevelVolumeNames([]byte(tc.body))
			if err != nil {
				t.Fatalf("TopLevelVolumeNames: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
