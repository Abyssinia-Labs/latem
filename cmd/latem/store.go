package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/appstore"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/playstore"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// A store check warns inside storeWarnWithin of an expiry and fails inside
// storeFailWithin. Both are for what actually breaks: an expired certificate
// stops new uploads and leaves live apps alone, and an expired TestFlight build
// stops opening on testers' phones.
const (
	storeWarnWithin = 30 * 24 * time.Hour
	storeFailWithin = 7 * 24 * time.Hour

	// storeHTTPTimeout bounds one request. A check that hangs on a store
	// answers nothing, and these answer in about a second.
	storeHTTPTimeout = 20 * time.Second
)

// storeMatchPassword opens the signing store, by the name docs/ios.md uses.
const storeMatchPassword = "MATCH_PASSWORD" //nolint:gosec // a secret's name, not a secret

// The secrets a store job reads, by the names docs/android.md uses. A check
// never prints a value, and resolves one only to use it.
var (
	storeAndroidSecrets = []string{
		"ANDROID_KEYSTORE_B64", "ANDROID_KEYSTORE_PASSWORD", "ANDROID_KEY_ALIAS", "ANDROID_KEY_PASSWORD",
		playstore.SecretServiceAccount,
	}
	// A Keychain item holds about 1,950 bytes of value through `security -i`,
	// and these two are usually more.
	storeLongSecrets = []string{"ANDROID_KEYSTORE_B64", playstore.SecretServiceAccount}
)

func newStoreCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Check an app's setup on the App Store and Google Play",
		Long: `Check an app's setup on the App Store and Google Play.

Signing material and store keys stay on your machines and in your own private
repository: this command reads them where they are, and sends nothing to a
control plane.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newStoreCheckCommand())
	return cmd
}

type storeCheckFlags struct {
	repo, env, app string
	bundleID, pkg  string
	track          string
	platform       string
	offline, json  bool
}

func newStoreCheckCommand() *cobra.Command {
	var f storeCheckFlags
	cmd := &cobra.Command{
		Use:   "check [path]",
		Short: "Say whether a preview would reach testers from this machine, and the fix for what would stop it",
		Long: `Say whether a preview would reach testers from this machine, and the fix
for what would stop it.

Read-only. It creates nothing and changes nothing at Apple, at Google, or in
the signing store, so it is safe beside a running build: it asks App Store
Connect with GET requests alone, and it never opens a Play edit, because a new
edit would invalidate one a running upload holds.

It reads an Expo project (default: the current directory, or the one Expo app
below it) for its bundle ID and package, and fastlane/Matchfile for its signing
store. app.config.ts is code, which latem does not run: give --bundle-id and
--package for an app that sets them there. Then, for each store:

  iOS      the secrets by name; the signing store and its branch; the App
           Store Connect key (latem doctor's agreement check); the app record;
           the App Store profile and its certificate, with their expiry; and
           the last upload
  Android  the secrets by name; the upload key's password, alias and expiry,
           read with keytool; the service account; and the last release on
           the track

A date warns inside 30 days and fails inside 7. Secrets are looked up as a run
looks them up: the repository's and environment's, then the repository's, then
the machine's. The checkout is matched to a [repos.*] entry by its origin
remote, or by --repo. With --repo and no path there is no checkout to give: the
repository's default branch is read without one (A57), or with --offline from
the agent's mirror, which this command reads and never writes.

Only names, ids, dates and Apple's and Google's own messages are printed; a
value never is (INV-4). --json prints the report for a script; the exit code
is the same.

Exit codes:
  0  nothing here would stop a preview; warnings do not change this
  1  at least one check failed, or the project could not be read
  2  usage`,
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.offline && (f.repo == "" || len(args) == 1) {
				return usageErrorf("--offline reads a repository without a checkout: give --repo and no path")
			}
			if !slices.Contains([]string{"", "ios", "android"}, f.platform) {
				return usageErrorf("--platform %q: one of ios, android", f.platform)
			}
			return runStoreCheck(cmd, args, f)
		},
	}
	cmd.Flags().StringVar(&f.repo, "repo", "", "the [repos.*] entry whose secrets to use; with no path, read without a checkout (A57)")
	cmd.Flags().StringVar(&f.env, "env", "", "the GitHub environment whose secrets to use (needs a repository)")
	cmd.Flags().StringVar(&f.app, "app", "", "the app's directory, when the tree has several")
	cmd.Flags().StringVar(&f.bundleID, "bundle-id", "", "the iOS bundle ID, instead of reading app.json")
	cmd.Flags().StringVar(&f.pkg, "package", "", "the Android package, instead of reading app.json")
	cmd.Flags().StringVar(&f.track, "track", "internal", "the Play track to read")
	cmd.Flags().StringVar(&f.platform, "platform", "", "check only ios or android (default: every platform the project has)")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "with --repo and no path: read the agent's mirror, not GitHub")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the report as JSON")
	return cmd
}

func runStoreCheck(cmd *cobra.Command, args []string, f storeCheckFlags) error {
	ctx := cmd.Context()

	// A machine with no config can still answer, for its machine-wide names.
	// One that does not parse cannot: what it says about this repository's
	// secrets is unknown, and the report would call them missing.
	cfg, err := loadConfig()
	if err != nil {
		if _, statErr := os.Stat(configFile()); !errors.Is(statErr, fs.ErrNotExist) {
			return err
		}
		cfg = &config.Config{Paths: config.DerivePaths(configDir())}
	}

	tree, err := openStoreTree(ctx, cfg, args, f)
	if err != nil {
		return err
	}
	defer tree.close()

	project, err := storeProjectFor(tree.fsys, f)
	if err != nil {
		return err
	}
	if tree.repoKey == "" && f.env != "" {
		return usageErrorf("--env needs a repository: give --repo, or a checkout of one this machine has")
	}

	s := newStoreChecker(project, tree, f)
	rep := s.run(ctx)

	out := cmd.OutOrStdout()
	if f.json {
		if err := printStoreJSON(out, s, rep); err != nil {
			return err
		}
	} else {
		printReport(out, rep, false, useColor(out))
	}
	if _, _, failed, _ := rep.counts(); failed > 0 {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%d check(s) failed", failed)}
	}
	return nil
}

// storeTree is the files a check reads, and the [repos.*] entry they are.
type storeTree struct {
	fsys    fs.FS
	where   string
	repoKey string
	slug    string // owner/name, or ""
	close   func()
}

func openStoreTree(ctx context.Context, cfg *config.Config, args []string, f storeCheckFlags) (storeTree, error) {
	if len(args) == 0 && f.repo != "" {
		target, err := resolveCheckTarget(ctx, cfg, "", workflowsCheckFlags{repo: f.repo})
		if err != nil {
			return storeTree{}, err
		}
		from := fromRemote
		if f.offline {
			from = fromMirror
		}
		files, err := readRepo(ctx, cfg, target.repoKey, from)
		if err != nil {
			return storeTree{}, err
		}
		return storeTree{files.fsys, files.where, target.repoKey, target.slug, files.close}, nil
	}

	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return storeTree{}, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return storeTree{}, fmt.Errorf("%s is not a directory; give the path of a checkout", abs)
	}
	target, err := resolveCheckTarget(ctx, cfg, abs, workflowsCheckFlags{repo: f.repo})
	if err != nil {
		return storeTree{}, err
	}
	return storeTree{os.DirFS(abs), abs, target.repoKey, target.slug, func() {}}, nil
}

// storeProjectFor reads the project and lays the flags over it. With both
// identifiers given there is nothing that has to be an Expo project.
func storeProjectFor(fsys fs.FS, f storeCheckFlags) (storeProject, error) {
	dir := f.app
	if dir == "" && f.bundleID != "" && f.pkg != "" {
		dir = "."
	}
	project, err := readStoreProject(fsys, dir)
	if err != nil {
		return project, err
	}
	if f.bundleID != "" {
		project.bundleID = f.bundleID
	}
	if f.pkg != "" {
		project.pkg = f.pkg
	}
	return project, nil
}

// storeChecker is one run of the checks. It is a doctor, for the App Store
// Connect checks and every seam a doctor already has.
type storeChecker struct {
	*doctor

	project  storeProject
	where    string
	repoKey  string
	slug     string
	env      string
	track    string
	platform string

	newPlay func() *playstore.Client
	// readUploadKey is a seam: keytool is not on a test machine, and a test
	// must not depend on the host having a JDK.
	readUploadKey func(ctx context.Context, keystoreB64, password []byte, alias string) (playstore.UploadKey, error)
}

func newStoreChecker(project storeProject, tree storeTree, f storeCheckFlags) *storeChecker {
	d := newDoctor()
	d.cfg, d.cfgErr = d.loadCfg()
	httpClient := &http.Client{Timeout: storeHTTPTimeout}
	d.newAppStore = func() *appstore.Client { return &appstore.Client{HTTP: httpClient} }
	return &storeChecker{
		doctor: d, project: project, where: tree.where, repoKey: tree.repoKey, slug: tree.slug,
		env: f.env, track: f.track, platform: f.platform,
		newPlay: func() *playstore.Client { return &playstore.Client{HTTP: httpClient} },
		readUploadKey: func(ctx context.Context, keystoreB64, password []byte, alias string) (playstore.UploadKey, error) {
			keytool := playstore.FindKeytool(d.sys.Getenv("JAVA_HOME"), playstore.DefaultKegRoots(), d.sys.LookPath)
			return playstore.ReadUploadKey(ctx, keytool, keystoreB64, password, alias)
		},
	}
}

// run makes every check it can, as doctor does: a person fixes the whole list
// in one sitting. A check that needs something an earlier one found missing
// says so as a skip, and with no usable identifier there is nothing to ask.
func (s *storeChecker) run(ctx context.Context) report {
	rep := report{}
	project := s.checkProject()
	rep.section("Project", project...)
	if anyFailed(project) {
		return rep
	}

	chain, err := s.resolver()
	if err != nil {
		rep.section("Secrets", fail("store.secrets", "secrets chain", err.Error()))
		return rep
	}
	if s.wants("ios") {
		rep.section("iOS", s.checkIOS(ctx, chain)...)
	}
	if s.wants("android") {
		rep.section("Android", s.checkAndroid(ctx, chain)...)
	}
	return rep
}

func (s *storeChecker) wants(platform string) bool {
	if s.platform != "" && s.platform != platform {
		return false
	}
	if platform == "ios" {
		return s.project.bundleID != ""
	}
	return s.project.pkg != ""
}

func (s *storeChecker) checkProject() []check {
	p := s.project
	label := s.where
	if p.dir != "" && p.dir != "." {
		label = filepath.Join(s.where, p.dir)
	}
	var out []check
	name := p.name
	if name == "" {
		name = "a project"
	}
	out = append(out, pass("store.project", "project", fmt.Sprintf("%s at %s", name, label)), s.checkRepoEntry())

	dynamicFix := []string{"latem store check --bundle-id <id> --package <name>"}
	identifier := func(id, title, value string) {
		switch {
		case value == "" && s.platform == strings.TrimPrefix(id, "store.identifier."):
			out = append(out, fail(id, title, "the project does not say", dynamicFix...))
		case value == "":
			out = append(out, skip(id, title, "the project does not say, so this platform is not checked"))
		case !storeIdentifier.MatchString(value):
			out = append(out, fail(id, title, fmt.Sprintf("%q is not a reverse-DNS identifier", value), dynamicFix...))
		default:
			out = append(out, pass(id, title, value))
		}
	}
	identifier("store.identifier.ios", "iOS bundle ID", p.bundleID)
	identifier("store.identifier.android", "Android package", p.pkg)

	if p.bundleID == "" && p.pkg == "" {
		c := fail("store.identifiers", "identifiers", "this project names no bundle ID and no package", dynamicFix...)
		if p.dynamic != "" {
			c = c.withNotes(p.dynamic + " is code, and latem does not run a project's code to read a value out of it.")
		}
		out = append(out, c)
	}
	return out
}

// checkRepoEntry says whose secrets the rest of the report is about. A machine
// that does not build this repository answers for its machine-wide names alone,
// and a report full of missing secrets should say that is why.
func (s *storeChecker) checkRepoEntry() check {
	const id, title = "store.repo", "repository"
	if s.repoKey != "" {
		return pass(id, title, fmt.Sprintf("[repos.%s] on this machine, whose secrets are read first", s.repoKey))
	}
	repo := s.slug
	if repo == "" {
		repo = "<owner/name>"
	}
	return warn(id, title, "this machine has no [repos.*] entry for it, so only machine-wide secrets were read",
		"latem repo add "+repo)
}

// resolver is the secrets chain, looked up the way a run looks up a name (A50).
func (s *storeChecker) resolver() (secretResolver, error) {
	paths := config.DerivePaths(s.dir)
	opts := secrets.Options{FilePath: paths.SecretsEnv}
	if s.cfg != nil {
		opts.FilePath, opts.Commands = s.cfg.Paths.SecretsEnv, s.cfg.Secrets.Commands
	}
	chain, err := s.newSecrets(opts)
	if err != nil {
		return nil, err
	}
	scopes := [][]string{nil}
	if s.repoKey != "" {
		scopes = [][]string{{s.repoKey}, nil}
		if s.env != "" {
			scopes = append([][]string{{s.repoKey, s.env}}, scopes...)
		}
	}
	return scopedResolver{secretResolver: chain, scopes: scopes}, nil
}

// scopedResolver answers a bare name from the most specific scope that has it.
type scopedResolver struct {
	secretResolver
	scopes [][]string
}

func (r scopedResolver) Resolve(ctx context.Context, name string) (secrets.Value, error) {
	for _, scope := range r.scopes {
		candidate := secrets.ScopedName(name, scope...)
		if secrets.ValidName(candidate) != nil {
			continue // a repository key no store can scope a name with
		}
		value, err := r.secretResolver.Resolve(ctx, candidate)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			continue
		case err != nil:
			return nil, err
		case len(value) == 0:
			continue
		}
		return value, nil
	}
	return nil, secrets.ErrNotFound
}

// checkNames says which of names this machine can answer, and nothing else: a
// value is resolved, measured and zeroed here.
func (s *storeChecker) checkNames(ctx context.Context, chain secretResolver, id string, names []string) check {
	const title = "secrets"
	var missing, unreadable []string
	for _, name := range names {
		value, err := chain.Resolve(ctx, name)
		value.Zero()
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			missing = append(missing, name)
		case err != nil:
			unreadable = append(unreadable, name)
		}
	}
	if len(missing) == 0 && len(unreadable) == 0 {
		return pass(id, title, fmt.Sprintf("%d of %d resolve here", len(names), len(names)))
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "no value here for "+strings.Join(missing, ", "))
	}
	if len(unreadable) > 0 {
		parts = append(parts, "could not read "+strings.Join(unreadable, ", "))
	}
	c := fail(id, title, strings.Join(parts, "; "))
	for _, name := range missing {
		c.Fix = append(c.Fix, s.setCommand(name))
	}
	if len(unreadable) > 0 {
		c = c.withNotes("A store answered with an error rather than saying it has no such name: a locked " +
			"Keychain, or a secrets.env that does not parse. `latem doctor --secrets` says which.")
	}
	return c
}

// setCommand is the command that stores name for the jobs this check is about.
func (s *storeChecker) setCommand(name string) string {
	sc := secretScope{repo: s.repoKey, env: s.env}
	cmd := "latem secrets set "
	if slices.Contains(storeLongSecrets, name) {
		cmd += "--store file "
	}
	return cmd + scopeArgs(sc, name)
}

// ------------------------------------------------------------------- iOS

func (s *storeChecker) checkIOS(ctx context.Context, chain secretResolver) []check {
	names := []string{appstore.SecretKeyID, appstore.SecretIssuerID, appstore.SecretP8}
	if s.project.match.url != "" {
		names = append([]string{storeMatchPassword}, names...)
	}
	out := []check{s.checkNames(ctx, chain, "store.ios.secrets", names)}
	out = append(out, s.checkMatchStore(ctx)...)

	const askID, askTitle = "store.ios.asc", "App Store Connect"
	creds, _, missing, unreadable := resolveAppStore(ctx, chain)
	defer creds.zero()
	if len(missing) > 0 || len(unreadable) > 0 {
		return append(out, skip(askID, askTitle, "not asking Apple without all three credentials"))
	}
	shape := s.appStoreShape(creds)
	out = append(out, shape...)
	if anyFailed(shape) {
		return append(out, skip(askID, askTitle, "not asking Apple while the values above are wrong"))
	}
	auth := s.appStoreAuth(ctx, creds.credentials())
	out = append(out, auth...)
	if anyFailed(auth) || (len(auth) > 0 && auth[0].Level == levelSkip) {
		return append(out, skip(askID, askTitle, "nothing more to ask until the key works"))
	}
	// The form fastlane uses. appStoreAuth has just proved it is this key's.
	token, err := appstore.Token(creds.credentials(), appstore.TeamKey, s.now())
	if err != nil {
		return append(out, fail(askID, askTitle, "could not mint a token: "+err.Error()))
	}
	return append(out, s.askApple(ctx, token)...)
}

// checkMatchStore asks whether this machine can read the signing store, and
// whether the branch is there. No clone: that would put encrypted signing
// material on a machine that may not otherwise have it, for no more answer.
func (s *storeChecker) checkMatchStore(ctx context.Context) []check {
	const id, title = "store.ios.match", "signing store"
	m := s.project.match
	if m.url == "" {
		return []check{skip(id, title, "no fastlane/Matchfile names one")}
	}
	if !storeURL.MatchString(m.url) {
		return []check{warn(id, title, fmt.Sprintf("%q is not an https or ssh URL, so it was not read", m.url))}
	}
	reach := s.checkRemote(ctx, id, title, "signing-store", m.url)
	if reach.Level != levelPass {
		return []check{reach}
	}
	// fastlane's own default, for a Matchfile that names no branch.
	branch := m.branch
	if branch == "" {
		branch = "master"
	}
	output, err := s.sys.Exec(ctx, "git", "ls-remote", "--heads", "--end-of-options", m.url, "refs/heads/"+branch)
	if err != nil {
		return []check{warn(id, title, fmt.Sprintf("%s answered, then would not list branch %s", m.url, branch))}
	}
	// The ref, not any output: Exec's output includes stderr, where ssh says
	// what it thinks of a new host.
	if !strings.Contains(string(output), "refs/heads/"+branch) {
		return []check{fail(id, title, fmt.Sprintf("%s has no branch %s", m.url, branch)).
			withManual("Run the import that creates it at a terminal, or fix git_branch in fastlane/Matchfile").
			withNotes("Runners read this store with match --readonly and never create what it holds.")}
	}
	return []check{pass(id, title, fmt.Sprintf("%s, branch %s", m.url, branch))}
}

// askApple is everything past authentication.
func (s *storeChecker) askApple(ctx context.Context, token string) []check {
	client := s.appStoreClient()
	bundle := s.project.bundleID

	var out []check
	app, found, err := client.AppByBundleID(ctx, token, bundle)
	switch {
	case err != nil:
		out = append(out, appleRefused("store.ios.app", "app record", err))
	case !found:
		out = append(out, fail("store.ios.app", "app record", "no app in App Store Connect has bundle ID "+bundle).
			withManual("App Store Connect → Apps → + → New App, with bundle ID "+bundle).
			withNotes("Apple's API has no call that creates an app, so this one is by hand. If the app "+
				"exists, this key belongs to another team."))
	default:
		out = append(out, pass("store.ios.app", "app record", fmt.Sprintf("%s, app %s", app.Name, app.ID)))
	}

	out = append(out, s.checkSigning(ctx, client, token)...)

	if found {
		out = append(out, s.checkLastUpload(ctx, client, token, app))
	}
	return out
}

func (s *storeChecker) checkSigning(ctx context.Context, client *appstore.Client, token string) []check {
	const profileID, profileTitle = "store.ios.profile", "App Store profile"
	bundle := s.project.bundleID

	id, found, err := client.BundleIDByIdentifier(ctx, token, bundle)
	if err != nil {
		return []check{appleRefused(profileID, profileTitle, err)}
	}
	if !found {
		return []check{fail(profileID, profileTitle, "the bundle ID "+bundle+" is not registered").
			withManual("developer.apple.com → Certificates, Identifiers & Profiles → Identifiers → +")}
	}
	profile, ok := signingProfile(id.Profiles, bundle)
	if !ok {
		return []check{fail(profileID, profileTitle, "no App Store profile exists for "+bundle,
			"bundle exec fastlane match appstore   # at a terminal, not on a runner").
			withNotes("Runners read signing with match --readonly and never create it.")}
	}

	var out []check
	regenerate := func(c check) check {
		c.Fix = append(c.Fix, "bundle exec fastlane match appstore --force   # at a terminal, not on a runner")
		return c
	}
	if profile.State != "ACTIVE" {
		out = append(out, regenerate(fail(profileID, profileTitle,
			fmt.Sprintf("%s is %s", profile.Name, profile.State))))
	} else {
		out = append(out, s.expiry(profileID, profileTitle, profile.Name, profile.Expires, regenerate))
	}
	return append(out, s.checkCertificate(ctx, client, token, profile))
}

// signingProfile picks the profile a build signs with: match's, by the name
// match gives it, or the App Store profile that lasts longest. EAS's sits beside
// match's on an app that moved, and an old one is no answer about this build.
func signingProfile(profiles []appstore.Profile, bundle string) (appstore.Profile, bool) {
	var best appstore.Profile
	found := false
	for _, p := range profiles {
		if p.Type != "IOS_APP_STORE" {
			continue
		}
		if p.Name == "match AppStore "+bundle {
			return p, true
		}
		better := p.State == "ACTIVE" && best.State != "ACTIVE"
		if p.State == best.State && p.Expires.After(best.Expires) {
			better = true
		}
		if !found || better {
			best, found = p, true
		}
	}
	return best, found
}

func (s *storeChecker) checkCertificate(ctx context.Context, client *appstore.Client, token string, profile appstore.Profile) check {
	const id, title = "store.ios.certificate", "distribution certificate"
	certs, err := client.ProfileCertificates(ctx, token, profile.ID)
	if err != nil {
		return appleRefused(id, title, err)
	}
	var best appstore.Certificate
	for _, c := range certs {
		if c.Type != "DISTRIBUTION" && c.Type != "IOS_DISTRIBUTION" {
			continue
		}
		if best.ID == "" || c.Expires.After(best.Expires) {
			best = c
		}
	}
	if best.ID == "" {
		return fail(id, title, profile.Name+" lists no distribution certificate that still exists",
			"bundle exec fastlane match appstore --force   # at a terminal, not on a runner")
	}
	what := fmt.Sprintf("%s (%s, %s)", best.Name, best.Type, best.ID)
	return s.expiry(id, title, what, best.Expires, func(c check) check {
		return c.withManual("At a terminal, not on a runner: make a new distribution certificate, then " +
			"`fastlane match import --type appstore` and `fastlane match appstore --force`").
			withNotes("An expired certificate stops new uploads and leaves live apps alone. A team may " +
				"hold few of them, so latem never makes or revokes one for you.")
	})
}

func (s *storeChecker) checkLastUpload(ctx context.Context, client *appstore.Client, token string, app appstore.App) check {
	const id, title = "store.ios.build", "last upload"
	build, found, err := client.LatestBuild(ctx, token, app.ID)
	switch {
	case err != nil:
		return appleRefused(id, title, err)
	case !found:
		return warn(id, title, "nothing has been uploaded to this app yet")
	}
	what := fmt.Sprintf("build %s, uploaded %s, %s", build.Version, shortDate(build.Uploaded), build.State)
	if build.State == "FAILED" || build.State == "INVALID" {
		return fail(id, title, what).
			withManual("App Store Connect → TestFlight → the build, for Apple's reason; then push a new one")
	}
	if build.Expired {
		return fail(id, title, what+", expired").withManual("Run the preview workflow: testers need a newer build")
	}
	return s.expiry(id, title, what, build.Expires, func(c check) check {
		return c.withManual("Run the preview workflow: a TestFlight build stops opening after 90 days")
	})
}

// storeNotAnswering is a store having a bad minute, which says nothing about
// this project's setup and must not fail it.
func storeNotAnswering(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// appleRefused turns a read that failed into the check it was for.
func appleRefused(id, title string, err error) check {
	e, ok := appstore.AsAPIError(err)
	switch {
	case !ok:
		return skip(id, title, "could not reach Apple: "+firstLineOf(err.Error()))
	case e.AgreementMissing():
		return fail(id, title, "Apple refused: "+e.Error()).
			withManual("The Account Holder accepts it in App Store Connect → Business").
			withNotes("Only the Account Holder can accept an agreement, and latem never does. Apple's " +
				"API cannot list them, so this refusal is the only way one shows.")
	case e.Status == http.StatusForbidden:
		return fail(id, title, "Apple refused: "+e.Error(),
			"# raise this key's role in App Store Connect → Users and Access → Integrations")
	case storeNotAnswering(e.Status):
		return warn(id, title, "Apple did not answer: "+e.Error())
	default:
		return fail(id, title, "Apple refused: "+e.Error())
	}
}

// --------------------------------------------------------------- Android

func (s *storeChecker) checkAndroid(ctx context.Context, chain secretResolver) []check {
	out := []check{
		s.checkNames(ctx, chain, "store.android.secrets", storeAndroidSecrets),
		s.checkUploadKey(ctx, chain),
	}
	return append(out, s.askPlay(ctx, chain)...)
}

func (s *storeChecker) checkUploadKey(ctx context.Context, chain secretResolver) check {
	const id, title = "store.android.keystore", "upload key"
	values := map[string]secrets.Value{}
	defer func() {
		for _, v := range values {
			v.Zero()
		}
	}()
	for _, name := range []string{"ANDROID_KEYSTORE_B64", "ANDROID_KEYSTORE_PASSWORD", "ANDROID_KEY_ALIAS"} {
		value, err := chain.Resolve(ctx, name)
		if err != nil {
			return skip(id, title, "not reading the keystore without its three values")
		}
		values[name] = value
	}
	key, err := s.readUploadKey(ctx, values["ANDROID_KEYSTORE_B64"], values["ANDROID_KEYSTORE_PASSWORD"],
		strings.TrimSpace(string(values["ANDROID_KEY_ALIAS"])))
	switch {
	case errors.Is(err, playstore.ErrNoKeytool):
		return skip(id, title, "no JDK here to read it with, so its password and expiry were not checked")
	case errors.Is(err, playstore.ErrKeystorePassword):
		return fail(id, title, "ANDROID_KEYSTORE_PASSWORD does not open ANDROID_KEYSTORE_B64",
			s.setCommand("ANDROID_KEYSTORE_PASSWORD"))
	case errors.Is(err, playstore.ErrNoAlias):
		return fail(id, title, "ANDROID_KEY_ALIAS names no key in the keystore", s.setCommand("ANDROID_KEY_ALIAS"))
	case err != nil:
		return fail(id, title, "ANDROID_KEYSTORE_B64: "+err.Error(), s.setCommand("ANDROID_KEYSTORE_B64"))
	}
	return s.expiry(id, title, "the password opens it and the alias is in it", key.Expires, func(c check) check {
		return c.withManual("Play Console → Test and release → App integrity → request an upload key reset")
	})
}

// askPlay asks whether Google takes this key, and what is on the track.
func (s *storeChecker) askPlay(ctx context.Context, chain secretResolver) []check {
	const accountID, accountTitle = "store.android.account", "service account"
	const trackID = "store.android.track"
	trackTitle := "track " + s.track

	value, err := chain.Resolve(ctx, playstore.SecretServiceAccount)
	if err != nil {
		return []check{skip(accountID, accountTitle, "not asking Google without "+playstore.SecretServiceAccount)}
	}
	defer value.Zero()
	account, err := playstore.ParseServiceAccount(value)
	if err != nil {
		return []check{fail(accountID, accountTitle, playstore.SecretServiceAccount+": "+err.Error(),
			s.setCommand(playstore.SecretServiceAccount))}
	}

	client := s.newPlay()
	token, err := client.Token(ctx, account, s.now())
	if err != nil {
		var refused *playstore.Error
		if !errors.As(err, &refused) {
			return []check{skip(accountID, accountTitle, "could not reach Google: "+firstLineOf(err.Error()))}
		}
		return []check{fail(accountID, accountTitle, "Google refused the key: "+refused.Error()).
			withManual(fmt.Sprintf("Google Cloud → IAM → Service accounts → %s → Keys: this key (%s) may be "+
				"deleted or disabled; make a new one, then `%s`", account.Email, account.KeyID,
				s.setCommand(playstore.SecretServiceAccount)))}
	}
	out := []check{pass(accountID, accountTitle, account.Email)}

	releases, err := client.Releases(ctx, token, s.project.pkg, s.track)
	if err != nil {
		var refused *playstore.Error
		if !errors.As(err, &refused) {
			return append(out, skip(trackID, trackTitle, "could not reach Google: "+firstLineOf(err.Error())))
		}
		return append(out, s.playRefused(trackID, trackTitle, account.Email, refused))
	}
	return append(out, s.lastRelease(trackID, trackTitle, releases))
}

func (s *storeChecker) playRefused(id, title, email string, refused *playstore.Error) check {
	pkg := s.project.pkg
	switch refused.Status {
	case http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound:
		return fail(id, title, "Google refused: "+refused.Error()).
			withManual("Play Console → Users and permissions → invite "+email+" with release rights on "+pkg,
				"If the app is not there yet: Play Console → Create app, with package "+pkg).
			withNotes("Google's API has no call that creates a normal app, and an invite made in Play " +
				"Console can take a while to reach the API.")
	}
	if storeNotAnswering(refused.Status) {
		return warn(id, title, "Google did not answer: "+refused.Error())
	}
	return fail(id, title, "Google refused: "+refused.Error())
}

// lastRelease reads what testers would get. Play gives a release no timestamp,
// so the version code is the only "when" here.
func (s *storeChecker) lastRelease(id, title string, releases []playstore.Release) check {
	if len(releases) == 0 {
		return warn(id, title, "nothing has been released on this track yet")
	}
	newest, code := playstore.Release{}, -1
	for _, r := range releases {
		for _, v := range r.VersionCodes {
			if v > code {
				newest, code = r, v
			}
		}
	}
	if code < 0 {
		newest = releases[0]
	}
	state := strings.ToLower(strings.TrimPrefix(newest.State, "RELEASE_LIFECYCLE_STATE_"))
	what := strings.ReplaceAll(state, "_", " ")
	if code >= 0 {
		what = fmt.Sprintf("version code %d, %s", code, what)
	}
	switch {
	case newest.Published():
		return pass(id, title, what)
	case newest.State == "RELEASE_LIFECYCLE_STATE_NOT_APPROVED":
		return fail(id, title, what).withManual("Play Console → Publishing overview, for Google's reason")
	default:
		return warn(id, title, what).
			withNotes("Testers do not have this release yet. A draft means the upload did not ask for " +
				"--release_status completed, or the app has not finished Play Console's setup.")
	}
}

// ---------------------------------------------------------------- output

// expiry levels something with a date, and passes anything worth saying about
// fixing it through remedy, which is not called for a pass.
func (s *storeChecker) expiry(id, title, what string, expires time.Time, remedy func(check) check) check {
	left := expires.Sub(s.now())
	switch {
	case expires.IsZero():
		return warn(id, title, what+", with no expiry date to read")
	case left <= 0:
		return remedy(fail(id, title, fmt.Sprintf("%s, expired %s", what, shortDate(expires))))
	case left <= storeFailWithin:
		return remedy(fail(id, title, fmt.Sprintf("%s, expires %s, in %s", what, shortDate(expires), daysLeft(left))))
	case left <= storeWarnWithin:
		return remedy(warn(id, title, fmt.Sprintf("%s, expires %s, in %s", what, shortDate(expires), daysLeft(left))))
	default:
		return pass(id, title, fmt.Sprintf("%s, expires %s", what, shortDate(expires)))
	}
}

func shortDate(t time.Time) string {
	if t.IsZero() {
		return "an unknown date"
	}
	return t.UTC().Format("2006-01-02")
}

func daysLeft(d time.Duration) string {
	days := int(math.Ceil(d.Hours() / 24))
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// The report as JSON, for a script. Field names are snake_case and stable, a
// check's id is what `doctor` promises for its own, and a level is "pass",
// "warn", "fail" or "skip".

type storeJSON struct {
	Source   string            `json:"source"`
	RepoKey  string            `json:"repo_key,omitempty"`
	App      storeAppJSON      `json:"app"`
	Passed   int               `json:"passed"`
	Warned   int               `json:"warned"`
	Failed   int               `json:"failed"`
	Skipped  int               `json:"skipped"`
	Sections []storeSectionDoc `json:"sections"`
}

type storeAppJSON struct {
	Dir      string `json:"dir"`
	Name     string `json:"name,omitempty"`
	BundleID string `json:"bundle_id,omitempty"`
	Package  string `json:"package,omitempty"`
	Team     string `json:"team_id,omitempty"`
}

type storeSectionDoc struct {
	Name   string          `json:"name"`
	Checks []storeCheckDoc `json:"checks"`
}

type storeCheckDoc struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Level  string   `json:"level"`
	Detail string   `json:"detail"`
	Fix    []string `json:"fix,omitempty"`
	Manual []string `json:"manual,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

func printStoreJSON(w io.Writer, s *storeChecker, rep report) error {
	passed, warned, failed, skipped := rep.counts()
	doc := storeJSON{
		Source: s.where, RepoKey: s.repoKey,
		App: storeAppJSON{
			Dir: s.project.dir, Name: s.project.name, BundleID: s.project.bundleID,
			Package: s.project.pkg, Team: s.project.team,
		},
		Passed: passed, Warned: warned, Failed: failed, Skipped: skipped,
		Sections: []storeSectionDoc{},
	}
	for _, sec := range rep.Sections {
		sd := storeSectionDoc{Name: sec.Name}
		for _, c := range sec.Checks {
			sd.Checks = append(sd.Checks, storeCheckDoc{
				ID: c.ID, Title: c.Title, Level: levelName(c.Level), Detail: c.Detail,
				Fix: c.Fix, Manual: c.Manual, Notes: c.Notes,
			})
		}
		doc.Sections = append(doc.Sections, sd)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
