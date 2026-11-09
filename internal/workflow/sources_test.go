package workflow

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// antescript's deploy-production, cut down: indexnow, four needs down from
// sitemap-before, downloads what sitemap-before uploaded. Nothing between them
// uploads anything.
const sitemapDeploy = `
on: {workflow_dispatch: {}}
jobs:
  sitemap-before:
    runs-on: ubuntu-latest
    steps:
      - run: curl -sf https://example.com/sitemap.xml -o sitemap-before.xml
      - uses: actions/upload-artifact@v7
        with: {name: sitemap-before, path: sitemap-before.xml, retention-days: 1}
  convex:
    needs: sitemap-before
    runs-on: ubuntu-latest
    steps: [{run: deploy convex}]
  web:
    needs: convex
    runs-on: ubuntu-latest
    steps:
      - run: build
      - uses: actions/upload-artifact@v7
        with: {name: web-dist, path: dist}
  smoke:
    needs: web
    runs-on: ubuntu-latest
    steps: [{run: curl https://example.com}]
  indexnow:
    needs: smoke
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v8
        with: {name: sitemap-before}
      - run: bun indexnow.ts --before sitemap-before.xml
  unrelated:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: sitemap-before, path: other.xml}
`

func planDispatch(t *testing.T, files map[string]string, file string, opts PlanOptions) map[string]protocol.PlannedJob {
	t.Helper()
	ev := protocol.Event{Name: EventWorkflowDispatch, Ref: "refs/heads/main", Workflow: Dir + "/" + file}
	plan := mustPlan(t, files, ev, opts)
	if len(plan) != 1 || plan[0].Error != "" {
		t.Fatalf("plan: %+v", plan)
	}
	out := map[string]protocol.PlannedJob{}
	for _, pj := range plan[0].Jobs {
		out[pj.Key] = pj
	}
	return out
}

// A download is placed with the job that uploaded what it names, however far
// up its needs that job is, and with no other: not a job between them that
// uploads something else, nor one outside its needs that uploads the same
// name (A84).
func TestPlanArtifactsFromAJobFurtherUp(t *testing.T) {
	jobs := planDispatch(t, map[string]string{"deploy.yml": sitemapDeploy}, "deploy.yml", PlanOptions{})
	for key, pj := range jobs {
		var want []string
		if key == "indexnow" {
			want = []string{"sitemap-before"}
		}
		if !reflect.DeepEqual(pj.ArtifactsFrom, want) {
			t.Errorf("%s: artifacts_from %v, want %v", key, pj.ArtifactsFrom, want)
		}
		if pj.Error != "" {
			t.Errorf("%s: %s", key, pj.Error)
		}
	}
}

// A control plane older than 2.9 would put the job on any machine, and the
// artifact is on one, so the job fails by name instead, as a call did before
// 2.8. The rest of the run is planned as before.
func TestPlanArtifactsFromNeeds29(t *testing.T) {
	jobs := planDispatch(t, map[string]string{"deploy.yml": sitemapDeploy}, "deploy.yml",
		PlanOptions{NoPlacedArtifacts: true})
	pj := jobs["indexnow"]
	if pj.ArtifactsFrom != nil || !strings.Contains(pj.Error, "protocol 2.9 (A84)") ||
		!strings.Contains(pj.Error, "sitemap-before") {
		t.Errorf("indexnow: artifacts_from %v, error %q", pj.ArtifactsFrom, pj.Error)
	}
	for key, pj := range jobs {
		if key != "indexnow" && pj.Error != "" {
			t.Errorf("%s: %s", key, pj.Error)
		}
	}
}

// take-one's and wake-cloud's Mac release: both names read what is only known
// as the jobs run, a step's outputs on one side and needs on the other, so
// they may be the same artifact and the download is placed with the build.
// Two known names that differ are not.
func TestPlanArtifactsFromMatchesNames(t *testing.T) {
	const macRelease = `
on: {workflow_dispatch: {}}
jobs:
  build:
    runs-on: blacksmith-6vcpu-macos-26
    outputs: {version: '${{ steps.version.outputs.version }}'}
    steps:
      - id: version
        run: echo version=1 >> "$GITHUB_OUTPUT"
      - id: dmg
        run: make dmg
      - uses: actions/upload-artifact@v7
        with:
          name: takeone-mac-${{ steps.version.outputs.version }}-${{ github.run_number }}
          path: ${{ steps.dmg.outputs.path }}
          if-no-files-found: error
  notes:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: release-notes, path: notes.md}
  publish:
    needs: [build, notes]
    runs-on: blacksmith-4vcpu-ubuntu-2404
    steps:
      - uses: actions/download-artifact@v8
        with:
          name: takeone-mac-${{ needs.build.outputs.version }}-${{ github.run_number }}
          path: ${{ runner.temp }}/dmg
  changelog:
    needs: notes
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v8
        with: {name: changes}
  everything:
    needs: [build, notes]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v8
  single:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with: {path: out/app.zip, archive: false}
  unzip:
    needs: single
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v8
        with: {name: app.zip}
`
	jobs := planDispatch(t, map[string]string{"mac-release.yml": macRelease}, "mac-release.yml", PlanOptions{})
	want := map[string][]string{
		// The DMG's name reads both jobs' runs, so it could be anything
		// build uploads; notes uploads release-notes, which is something
		// else only if the names are compared, and they cannot be.
		"publish": {"build", "notes"},
		// changes is no name notes uploads: nothing to place by, and the
		// compiler says so.
		"changelog": nil,
		// No name is every artifact of the run.
		"everything": {"build", "notes"},
		// archive: false names the artifact after its file.
		"unzip": {"single"},
	}
	for key, w := range want {
		if got := jobs[key].ArtifactsFrom; !reflect.DeepEqual(got, w) {
			t.Errorf("%s: artifacts_from %v, want %v", key, got, w)
		}
	}
}

// A download that an earlier step of its own job satisfies reads no other job,
// so it places nothing.
func TestPlanArtifactsFromNotForADownloadInItsOwnJob(t *testing.T) {
	const wf = `
on: {workflow_dispatch: {}}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: dist, path: dist}
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: dist, path: out}
      - uses: actions/download-artifact@v8
        with: {name: dist, path: back}
`
	jobs := planDispatch(t, map[string]string{"w.yml": wf}, "w.yml", PlanOptions{})
	if got := jobs["b"].ArtifactsFrom; got != nil {
		t.Errorf("b: artifacts_from %v, want none", got)
	}
}

// sokie's release: publish downloads what pack uploaded, and pack is a job of
// a workflow its caller calls, so the source is a job path (A82).
func TestPlanArtifactsFromACalledJob(t *testing.T) {
	files := maps.Clone(calls)
	files["package.yml"] = strings.Replace(files["package.yml"], "      - if: inputs.smoke && inputs.retries == 2\n",
		"      - uses: actions/upload-artifact@v7\n        with: {name: cli-package, path: '${{ runner.temp }}/pack'}\n"+
			"      - if: inputs.smoke && inputs.retries == 2\n", 1)
	files["release.yml"] = strings.Replace(files["release.yml"], "      - run: npm publish \"$TARBALL\"\n",
		"      - uses: actions/download-artifact@v8\n        with: {name: cli-package, path: '${{ runner.temp }}/pack'}\n"+
			"      - run: npm publish \"$TARBALL\"\n", 1)
	for _, pj := range dispatchDeploy(t, files, PlanOptions{}) {
		var want []string
		if pj.Key == "cli/publish" {
			want = []string{"cli/package/pack"}
		}
		if !reflect.DeepEqual(pj.ArtifactsFrom, want) {
			t.Errorf("%s: artifacts_from %v, want %v", pj.Key, pj.ArtifactsFrom, want)
		}
		for _, k := range pj.ArtifactsFrom {
			if !protocol.IsJobPath(k) {
				t.Errorf("%q is not a key the protocol carries", k)
			}
		}
	}
}

// The machine that runs a job works its artifacts_from out again from the
// commit, whatever the control plane it was planned for speaks.
func TestArtifactsFromIsTheCommitsAnswer(t *testing.T) {
	fsys := fleetFS(map[string]string{"deploy.yml": sitemapDeploy})
	ev := protocol.Event{Name: EventWorkflowDispatch, Ref: "refs/heads/main", SHA: planSHA, Workflow: Dir + "/deploy.yml"}
	from, err := ArtifactsFrom(fsys, Dir+"/deploy.yml", "indexnow", ev, PlanOptions{NoPlacedArtifacts: true})
	if err != nil || !reflect.DeepEqual(from, []string{"sitemap-before"}) {
		t.Errorf("indexnow: %v, %v", from, err)
	}
	if from, err := ArtifactsFrom(fsys, Dir+"/deploy.yml", "smoke", ev, PlanOptions{}); err != nil || from != nil {
		t.Errorf("smoke reads nothing: %v, %v", from, err)
	}
	if _, err := ArtifactsFrom(fsys, Dir+"/deploy.yml", "nope", ev, PlanOptions{}); err == nil {
		t.Error("a job the file does not plan should be an error")
	}
}
