//go:generate go install -v github.com/kevinburke/go-bindata/go-bindata
//go:generate go-bindata -prefix res/ -pkg assets -o assets/assets.go res/Firefox.lnk
//go:generate go install -v github.com/josephspurrier/goversioninfo/cmd/goversioninfo
//go:generate goversioninfo -icon=res/papp.ico -manifest=res/papp.manifest
package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/Jeffail/gabs"
	"github.com/pkg/errors"
	"github.com/portapps/phyrox-portable/assets"
	"github.com/portapps/portapps/v2"
	"github.com/portapps/portapps/v2/pkg/dialog"
	"github.com/portapps/portapps/v2/pkg/log"
	"github.com/portapps/portapps/v2/pkg/mutex"
	"github.com/portapps/portapps/v2/pkg/shortcut"
	"github.com/portapps/portapps/v2/pkg/utl"
)

type config struct {
	Profile           string `yaml:"profile" mapstructure:"profile"`
	MultipleInstances bool   `yaml:"multiple_instances" mapstructure:"multiple_instances"`
	Locale            string `yaml:"locale" mapstructure:"locale"`
	Cleanup           bool   `yaml:"cleanup" mapstructure:"cleanup"`
}

var (
	app *portapps.App
	cfg *config
)

const (
	defaultLocale = "en-US"
)

func init() {
	var err error

	// Default config
	cfg = &config{
		Profile:           "default",
		MultipleInstances: false,
		Locale:            defaultLocale,
		Cleanup:           false,
	}

	// Init app
	if app, err = portapps.NewWithCfg("phyrox-portable", "Phyrox", cfg); err != nil {
		log.Fatal().Err(err).Msg("Cannot initialize application. See log file for more info.")
	}
}

func main() {
	utl.CreateFolder(app.DataPath)
	profileFolder := utl.CreateFolder(app.DataPath, "profile", cfg.Profile)

	app.Process = utl.PathJoin(app.AppPath, "firefox.exe")
	app.Args = []string{
		"--profile",
		profileFolder,
	}

	// Set env vars
	crashreporterFolder := utl.CreateFolder(app.DataPath, "crashreporter")
	pluginsFolder := utl.CreateFolder(app.DataPath, "plugins")
	utl.OverrideEnv("MOZ_CRASHREPORTER", "0")
	utl.OverrideEnv("MOZ_CRASHREPORTER_DATA_DIRECTORY", crashreporterFolder)
	utl.OverrideEnv("MOZ_CRASHREPORTER_DISABLE", "1")
	utl.OverrideEnv("MOZ_CRASHREPORTER_NO_REPORT", "1")
	utl.OverrideEnv("MOZ_DATA_REPORTING", "0")
	utl.OverrideEnv("MOZ_MAINTENANCE_SERVICE", "0")
	utl.OverrideEnv("MOZ_PLUGIN_PATH", pluginsFolder)
	utl.OverrideEnv("MOZ_UPDATER", "0")

	// Create and check mutex
	mu, err := mutex.New(app.ID)
	defer mu.Release()
	if err != nil {
		if !cfg.MultipleInstances {
			log.Error().Msg("You have to enable multiple instances in your configuration if you want to launch another instance")
			if _, err = dialog.MsgBox(
				fmt.Sprintf("%s portable", app.Name),
				"Other instance detected. You have to enable multiple instances in your configuration if you want to launch another instance.",
				dialog.MsgBoxBtnOk|dialog.MsgBoxIconError); err != nil {
				log.Error().Err(err).Msg("Cannot create dialog box")
			}
			return
		} else {
			log.Warn().Msg("Another instance is already running")
		}
	}

	// Cleanup on exit
	if cfg.Cleanup {
		defer func() {
			utl.Cleanup([]string{
				path.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox"),
				path.Join(os.Getenv("LOCALAPPDATA"), "Mozilla", "Firefox"),
				path.Join(os.Getenv("USERPROFILE"), "AppData", "LocalLow", "Mozilla"),
			})
		}()
	}

	// Locale
	locale, err := checkLocale()
	if err != nil {
		log.Error().Err(err).Msg("Cannot set locale")
	}

	// Multiple instances
	if cfg.MultipleInstances {
		log.Info().Msg("Multiple instances enabled")
		app.Args = append(app.Args, "--no-remote")
	}

	// Policies
	if err := createPolicies(); err != nil {
		log.Fatal().Err(err).Msg("Cannot create policies")
	}

	// Autoconfig
	prefFolder := utl.CreateFolder(app.AppPath, "defaults/pref")
	autoconfig := utl.PathJoin(prefFolder, "autoconfig.js")
	if err := utl.CreateFile(autoconfig, `//
pref("general.config.filename", "portapps.cfg");
pref("general.config.obscure_value", 0);`); err != nil {
		log.Fatal().Err(err).Msg("Cannot write autoconfig.js")
	}

	// Mozilla cfg
	mozillaCfgPath := utl.PathJoin(app.AppPath, "portapps.cfg")
	mozillaCfgFile, err := os.Create(mozillaCfgPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Cannot create portapps.cfg")
	}
	mozillaCfgData := struct {
		Locale string
	}{
		locale,
	}
	mozillaCfgTpl := template.Must(template.New("mozillaCfg").Parse(`// Set locale
pref("intl.locale.requested", "{{ .Locale }}");

// Extensions scopes
lockPref("extensions.enabledScopes", 4);
lockPref("extensions.autoDisableScopes", 3);

// Don't show 'know your rights' on first run
pref("browser.rights.3.shown", true);

// Don't show WhatsNew on first run after every update
pref("browser.startup.homepage_override.mstone", "ignore");
`))
	if err := mozillaCfgTpl.Execute(mozillaCfgFile, mozillaCfgData); err != nil {
		log.Fatal().Err(err).Msg("Cannot write portapps.cfg")
	}

	// Fix extensions path
	if err := updateAddonStartup(profileFolder); err != nil {
		log.Error().Err(err).Msg("Cannot fix extensions path")
	}

	// Copy default shortcut
	shortcutPath := path.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Phyrox Portable.lnk")
	defaultShortcut, err := assets.Asset("Firefox.lnk")
	if err != nil {
		log.Error().Err(err).Msg("Cannot load asset Firefox.lnk")
	}
	err = ioutil.WriteFile(shortcutPath, defaultShortcut, 0644)
	if err != nil {
		log.Error().Err(err).Msg("Cannot write default shortcut")
	}

	// Update default shortcut
	err = shortcut.Create(shortcut.Shortcut{
		ShortcutPath:     shortcutPath,
		TargetPath:       app.Process,
		Arguments:        shortcut.Property{Clear: true},
		Description:      shortcut.Property{Value: "Phyrox Portable by Portapps"},
		IconLocation:     shortcut.Property{Value: app.Process},
		WorkingDirectory: shortcut.Property{Value: app.AppPath},
	})
	if err != nil {
		log.Error().Err(err).Msg("Cannot create shortcut")
	}
	defer func() {
		if err := os.Remove(shortcutPath); err != nil {
			log.Error().Err(err).Msg("Cannot remove shortcut")
		}
	}()

	defer app.Close()
	app.Launch(os.Args[1:])
}

func createPolicies() error {
	appFile := utl.PathJoin(utl.CreateFolder(app.AppPath, "distribution"), "policies.json")
	dataFile := utl.PathJoin(app.DataPath, "policies.json")
	defaultPolicies := struct {
		Policies map[string]interface{} `json:"policies"`
	}{
		Policies: map[string]interface{}{
			"DisableAppUpdate":        true,
			"DontCheckDefaultBrowser": true,
		},
	}

	jsonPolicies, err := gabs.Consume(defaultPolicies)
	if err != nil {
		return errors.Wrap(err, "Cannot consume default policies")
	}
	log.Debug().Msgf("Default policies: %s", jsonPolicies.String())

	if utl.Exists(dataFile) {
		rawCustomPolicies, err := ioutil.ReadFile(dataFile)
		if err != nil {
			return errors.Wrap(err, "Cannot read custom policies")
		}

		jsonPolicies, err = gabs.ParseJSON(rawCustomPolicies)
		if err != nil {
			return errors.Wrap(err, "Cannot consume custom policies")
		}
		log.Debug().Msgf("Custom policies: %s", jsonPolicies.String())

		jsonPolicies.Set(true, "policies", "DisableAppUpdate")
		jsonPolicies.Set(true, "policies", "DontCheckDefaultBrowser")
	}

	log.Debug().Msgf("Applied policies: %s", jsonPolicies.String())
	err = ioutil.WriteFile(appFile, []byte(jsonPolicies.StringIndent("", "  ")), 0644)
	if err != nil {
		return errors.Wrap(err, "Cannot write policies")
	}

	return nil
}

func checkLocale() (string, error) {
	extSourceFile := fmt.Sprintf("%s.xpi", cfg.Locale)
	extDestFile := fmt.Sprintf("langpack-%s@firefox.mozilla.org.xpi", cfg.Locale)
	extsFolder := utl.CreateFolder(app.AppPath, "distribution", "extensions")
	localeXpi := utl.PathJoin(app.AppPath, "langs", extSourceFile)

	// If default locale skip (already embedded)
	if cfg.Locale == defaultLocale {
		return cfg.Locale, nil
	}

	// Check .xpi file exists
	if !utl.Exists(localeXpi) {
		return defaultLocale, fmt.Errorf("XPI file does not exist in %s", localeXpi)
	}

	// Copy .xpi
	if err := utl.CopyFile(localeXpi, utl.PathJoin(extsFolder, extDestFile)); err != nil {
		return defaultLocale, err
	}

	return cfg.Locale, nil
}

func updateAddonStartup(profileFolder string) error {
	lz4File := path.Join(profileFolder, "addonStartup.json.lz4")
	if !utl.Exists(lz4File) || app.Prev.RootPath == "" {
		return nil
	}

	lz4Raw, err := mozLz4Decompress(lz4File)
	if err != nil {
		return err
	}

	prevPathLin := strings.Replace(utl.FormatUnixPath(app.Prev.RootPath), ` `, `%20`, -1)
	currPathLin := strings.Replace(utl.FormatUnixPath(app.RootPath), ` `, `%20`, -1)
	lz4Str := strings.Replace(string(lz4Raw), prevPathLin, currPathLin, -1)

	prevPathWin := strings.Replace(strings.Replace(utl.FormatWindowsPath(app.Prev.RootPath), `\`, `\\`, -1), ` `, `%20`, -1)
	currPathWin := strings.Replace(strings.Replace(utl.FormatWindowsPath(app.RootPath), `\`, `\\`, -1), ` `, `%20`, -1)
	lz4Str = strings.Replace(lz4Str, prevPathWin, currPathWin, -1)

	lz4Enc, err := mozLz4Compress([]byte(lz4Str))
	if err != nil {
		return err
	}

	return ioutil.WriteFile(lz4File, lz4Enc, 0644)
}
