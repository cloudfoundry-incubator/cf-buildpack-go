package finalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/cloudfoundry/go-buildpack/src/go/data"
	"github.com/cloudfoundry/go-buildpack/src/go/godep"
	"github.com/cloudfoundry/go-buildpack/src/go/warnings"
	"github.com/cloudfoundry/libbuildpack"
)

type Command interface {
	Execute(string, io.Writer, io.Writer, string, ...string) error
}

type Stager interface {
	BuildDir() string
	CacheDir() string
	ClearDepDir() error
	DepDir() string
	DepsIdx() string
	WriteProfileD(string, string) error
}

type Finalizer struct {
	Stager           Stager
	Command          Command
	Log              *libbuildpack.Logger
	VendorTool       string
	GoVersion        string
	Godep            godep.Godep
	MainPackageName  string
	GoPath           string
	PackageList      []string
	BuildFlags       []string
	VendorExperiment bool
}

func NewFinalizer(stager Stager, command Command, logger *libbuildpack.Logger) (*Finalizer, error) {
	config := struct {
		Config struct {
			GoVersion  string `yaml:"GoVersion"`
			VendorTool string `yaml:"VendorTool"`
			Godep      string `yaml:"Godep"`
		} `yaml:"config"`
	}{}
	if err := libbuildpack.NewYAML().Load(filepath.Join(stager.DepDir(), "config.yml"), &config); err != nil {
		logger.Error("Unable to read config.yml: %s", err.Error())
		return nil, err
	}

	var godep godep.Godep
	if config.Config.VendorTool == "godep" {
		if err := json.Unmarshal([]byte(config.Config.Godep), &godep); err != nil {
			logger.Error("Unable to load config Godep json: %s", err.Error())
			return nil, err
		}
	}

	return &Finalizer{
		Stager:     stager,
		Command:    command,
		Log:        logger,
		Godep:      godep,
		GoVersion:  config.Config.GoVersion,
		VendorTool: config.Config.VendorTool,
	}, nil
}

func Run(gf *Finalizer) error {
	if err := gf.SetGoCache(); err != nil {
		gf.Log.Error("Unable to print gocache location: %s", err.Error())
		return err
	}

	if err := gf.SetMainPackageName(); err != nil {
		gf.Log.Error("Unable to determine import path: %s", err.Error())
		return err
	}

	if err := os.MkdirAll(filepath.Join(gf.Stager.BuildDir(), "bin"), 0755); err != nil {
		gf.Log.Error("Unable to create <build-dir>/bin: %s", err.Error())
		return err
	}

	if gf.VendorTool != "gomod" {
		if err := gf.SetupGoPath(); err != nil {
			gf.Log.Error("Unable to setup Go path: %s", err.Error())
			return err
		}
	} else {
		if err := os.Setenv("GOBIN", filepath.Join(gf.Stager.BuildDir(), "bin")); err != nil {
			gf.Log.Error("Unable to setup GOBIN: %s", err.Error())
			return err
		}
	}

	if err := gf.HandleVendorExperiment(); err != nil {
		gf.Log.Error("Invalid vendor config: %s", err.Error())
		return err
	}

	if gf.VendorTool == "glide" {
		if err := gf.RunGlideInstall(); err != nil {
			gf.Log.Error("Error running 'glide install': %s", err.Error())
			return err
		}
	} else if gf.VendorTool == "dep" {
		if err := gf.RunDepEnsure(); err != nil {
			gf.Log.Error("Error running 'dep ensure': %s", err.Error())
			return err
		}
	}

	if err := gf.SetBuildFlags(); err != nil {
		gf.Log.Error("Unable to setup build flags:': %s", err.Error())
		return err
	}

	if err := gf.SetInstallPackages(); err != nil {
		gf.Log.Error("Unable to determine packages to install: %s", err.Error())
		return err
	}

	if err := gf.CompileApp(); err != nil {
		gf.Log.Error("Unable to compile application: %s", err.Error())
		return err
	}

	if err := gf.CreateStartupEnvironment("/tmp"); err != nil {
		gf.Log.Error("Unable to create startup scripts: %s", err.Error())
		return err
	}

	return nil
}

func (gf *Finalizer) SetMainPackageName() error {
	switch gf.VendorTool {
	case "godep":
		gf.MainPackageName = gf.Godep.ImportPath

	case "glide":
		buffer := new(bytes.Buffer)
		errorBuffer := new(bytes.Buffer)

		if err := gf.Command.Execute(gf.Stager.BuildDir(), buffer, errorBuffer, "glide", "name"); err != nil {
			gf.Log.Error("problem retrieving main package name: %s", errorBuffer)
			return err
		}
		gf.MainPackageName = strings.TrimSpace(buffer.String())
	case "dep":
		fallthrough
	case "go_nativevendoring":
		gf.MainPackageName = os.Getenv("GOPACKAGENAME")
		if gf.MainPackageName == "" {
			gf.Log.Error(warnings.NoGOPACKAGENAMEerror())
			return errors.New("GOPACKAGENAME unset")
		}
	case "gomod":
		buffer := new(bytes.Buffer)
		errorBuffer := new(bytes.Buffer)

		if err := gf.Command.Execute(gf.Stager.BuildDir(), buffer, errorBuffer, "go", "list", "-m"); err != nil {
			gf.Log.Error("problem retrieving main package name: %s", errorBuffer)
			return err
		}
		gf.MainPackageName = strings.TrimSpace(buffer.String())
	default:
		return errors.New("invalid vendor tool")
	}
	return nil
}

func (gf *Finalizer) SetGoCache() error {
	return os.Setenv("GOCACHE", filepath.Join(gf.Stager.CacheDir(), "go-cache"))
}

func (gf *Finalizer) SetupGoPath() error {
	var skipMoveFile = map[string]bool{
		".cloudfoundry": true,
		"Procfile":      true,
		".profile":      true,
		"src":           true,
		".profile.d":    true,
	}

	var goPath string
	goPathInImage := os.Getenv("GO_SETUP_GOPATH_IN_IMAGE") == "true"

	if goPathInImage {
		goPath = gf.Stager.BuildDir()
	} else {
		tmpDir, err := ioutil.TempDir("", "gobuildpack.gopath")
		if err != nil {
			return err
		}
		goPath = filepath.Join(tmpDir, ".go")
	}

	err := os.Setenv("GOPATH", goPath)
	if err != nil {
		return err
	}
	gf.GoPath = goPath

	binDir := filepath.Join(gf.Stager.BuildDir(), "bin")
	err = os.MkdirAll(binDir, 0755)
	if err != nil {
		return err
	}

	packageDir := gf.mainPackagePath()
	err = os.MkdirAll(packageDir, 0755)
	if err != nil {
		return err
	}

	if goPathInImage {
		files, err := ioutil.ReadDir(gf.Stager.BuildDir())
		if err != nil {
			return err
		}
		for _, f := range files {
			if !skipMoveFile[f.Name()] {
				src := filepath.Join(gf.Stager.BuildDir(), f.Name())
				dest := filepath.Join(packageDir, f.Name())

				err = os.Rename(src, dest)
				if err != nil {
					return err
				}
			}
		}
	} else {
		err = os.Setenv("GOBIN", binDir)
		if err != nil {
			return err
		}

		err = libbuildpack.CopyDirectory(gf.Stager.BuildDir(), packageDir)
		if err != nil {
			return err
		}
	}
	// unset git dir or it will mess with go install
	return os.Unsetenv("GIT_DIR")
}

func unescapeBuildFlagString(s string) string {
	return strings.ReplaceAll(s, `\,`, `,`)
}

func unescapeBuildFlagStringSlice(str []string) []string {
	var ret []string
	for _, s := range str {
		ret = append(ret, unescapeBuildFlagString(s))
	}
	return ret
}

func (gf *Finalizer) SetBuildFlags() error {
	flags := []string{"-tags", "cloudfoundry", "-buildmode", "pie"}

	if os.Getenv("GO_LINKER_SYMBOL") != "" && os.Getenv("GO_LINKER_VALUE") != "" {
		// regex to split on comma but allow escaping with backslash
		r := regexp.MustCompile(`(?m)(?:\\.|[^,\\]+)*`)

		// derive the linker symbols/values from the environment
		linkerSymbols := r.FindAllString(os.Getenv("GO_LINKER_SYMBOL"), -1)
		linkerSymbols = unescapeBuildFlagStringSlice(linkerSymbols)
		linkerValues := r.FindAllString(os.Getenv("GO_LINKER_VALUE"), -1)
		linkerValues = unescapeBuildFlagStringSlice(linkerValues)
		if len(linkerSymbols) != len(linkerValues) {
			return errors.New("linker symbol value length mismatch")
		}

		// generate the linker symbol/value pairs
		var linkerString strings.Builder
		linkerString.WriteString(`"`) // start it with a quote
		for i := 0; i < len(linkerSymbols); i++ {
			linkerString.WriteString(fmt.Sprintf("-X '%s=%s' ", linkerSymbols[i], linkerValues[i]))
		}
		linkerString.WriteString(`"`) // end it with a quote

		// append the linker symbol/value string to the ldflags
		ld_flags := []string{"-ldflags", linkerString.String()}

		// append the ld_flags to the other build flags
		flags = append(flags, ld_flags...)
	}

	gf.BuildFlags = flags
	return nil
}

func (gf *Finalizer) RunDepEnsure() error {
	vendorDirExists, err := libbuildpack.FileExists(filepath.Join(gf.mainPackagePath(), "vendor"))
	if err != nil {
		return err
	}
	runEnsure := true

	if vendorDirExists {
		numSubDirs := 0
		files, err := ioutil.ReadDir(filepath.Join(gf.mainPackagePath(), "vendor"))
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() {
				numSubDirs++
			}
		}

		if numSubDirs > 0 {
			runEnsure = false
		}
	}

	if runEnsure {
		gf.Log.BeginStep("Fetching any unsaved dependencies (dep ensure)")

		if err := gf.Command.Execute(gf.mainPackagePath(), os.Stdout, os.Stderr, "dep", "ensure"); err != nil {
			return err
		}
	} else {
		gf.Log.Info("Note: skipping (dep ensure) due to non-empty vendor directory.")
	}

	return nil
}

func (gf *Finalizer) RunGlideInstall() error {
	if gf.VendorTool != "glide" {
		return nil
	}

	vendorDirExists, err := libbuildpack.FileExists(filepath.Join(gf.mainPackagePath(), "vendor"))
	if err != nil {
		return err
	}
	runGlideInstall := true

	if vendorDirExists {
		numSubDirs := 0
		files, err := ioutil.ReadDir(filepath.Join(gf.mainPackagePath(), "vendor"))
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() {
				numSubDirs++
			}
		}

		if numSubDirs > 0 {
			runGlideInstall = false
		}
	}

	if runGlideInstall {
		gf.Log.BeginStep("Fetching any unsaved dependencies (glide install)")

		if err := gf.Command.Execute(gf.mainPackagePath(), os.Stdout, os.Stderr, "glide", "install"); err != nil {
			return err
		}
	} else {
		gf.Log.Info("Note: skipping (glide install) due to non-empty vendor directory.")
	}

	return nil
}

func (gf *Finalizer) HandleVendorExperiment() error {
	gf.VendorExperiment = true

	if os.Getenv("GO15VENDOREXPERIMENT") == "" {
		return nil
	}

	ver, err := semver.NewVersion(gf.GoVersion)
	if err != nil {
		return err
	}

	go16 := ver.Major() == 1 && ver.Minor() == 6
	if !go16 {
		gf.Log.Error(warnings.UnsupportedGO15VENDOREXPERIMENTerror())
		return errors.New("unsupported GO15VENDOREXPERIMENT")
	}

	if os.Getenv("GO15VENDOREXPERIMENT") == "0" {
		gf.VendorExperiment = false
	}

	return nil
}

func (gf *Finalizer) SetInstallPackages() error {
	var packages []string

	if os.Getenv("GO_INSTALL_PACKAGE_SPEC") != "" {
		packages = append(packages, strings.Split(os.Getenv("GO_INSTALL_PACKAGE_SPEC"), " ")...)
	}

	vendorDirExists, err := libbuildpack.FileExists(filepath.Join(gf.mainPackagePath(), "vendor"))
	if err != nil {
		return err
	}

	if gf.VendorTool == "godep" {
		useVendorDir := gf.VendorExperiment && !gf.Godep.WorkspaceExists

		if gf.Godep.WorkspaceExists && vendorDirExists {
			gf.Log.Warning(warnings.GodepsWorkspaceWarning())
		}

		if useVendorDir && !vendorDirExists {
			gf.Log.Warning("vendor/ directory does not exist.")
		}

		if len(packages) != 0 {
			gf.Log.Warning(warnings.PackageSpecOverride(packages))
		} else if len(gf.Godep.Packages) != 0 {
			packages = gf.Godep.Packages
		} else {
			gf.Log.Warning("Installing package '.' (default)")
			packages = append(packages, ".")
		}

		if useVendorDir {
			packages = gf.updatePackagesForVendor(packages)
		}
	} else {
		if !gf.VendorExperiment && gf.VendorTool == "go_nativevendoring" {
			gf.Log.Error(warnings.MustUseVendorError())
			return errors.New("must use vendor/ for go native vendoring")
		}

		if len(packages) == 0 {
			packages = append(packages, ".")
			gf.Log.Warning("Installing package '.' (default)")
		}

		packages = gf.updatePackagesForVendor(packages)
	}

	gf.PackageList = packages
	return nil
}

func (gf *Finalizer) CompileApp() error {
	cmd := "go"
	args := []string{"install"}
	args = append(args, gf.BuildFlags...)
	args = append(args, gf.PackageList...)

	if gf.VendorTool == "godep" && (gf.Godep.WorkspaceExists || !gf.VendorExperiment) {
		args = append([]string{"go"}, args...)
		cmd = "godep"
	}

	gf.Log.BeginStep(fmt.Sprintf("Running: %s %s", cmd, strings.Join(args, " ")))

	err := gf.Command.Execute(gf.mainPackagePath(), os.Stdout, os.Stderr, cmd, args...)
	if err != nil {
		return err
	}
	return nil
}

func (gf *Finalizer) CreateStartupEnvironment(tempDir string) error {
	mainPkgName := gf.MainPackageName
	if len(gf.PackageList) > 0 && gf.PackageList[0] != "." {
		mainPkgName = filepath.Base(gf.PackageList[0])
	}

	err := ioutil.WriteFile(filepath.Join(tempDir, "buildpack-release-step.yml"), []byte(data.ReleaseYAML(mainPkgName)), 0644)
	if err != nil {
		gf.Log.Error("Unable to write release yml: %s", err.Error())
		return err
	}

	if os.Getenv("GO_INSTALL_TOOLS_IN_IMAGE") == "true" {
		goRuntimeLocation := filepath.Join("$DEPS_DIR", gf.Stager.DepsIdx(), "go"+gf.GoVersion, "go")

		gf.Log.BeginStep("Leaving go tool chain in $GOROOT=%s", goRuntimeLocation)

	} else {
		if err := gf.Stager.ClearDepDir(); err != nil {
			return err
		}
	}

	if os.Getenv("GO_SETUP_GOPATH_IN_IMAGE") == "true" {
		gf.Log.BeginStep("Cleaning up $GOPATH/pkg")
		if err := os.RemoveAll(filepath.Join(gf.GoPath, "pkg")); err != nil {
			return err
		}

		if err := gf.Stager.WriteProfileD("zzgopath.sh", data.ZZGoPathScript(mainPkgName)); err != nil {
			return err
		}
	}

	return gf.Stager.WriteProfileD("go.sh", data.GoScript())
}

func (gf *Finalizer) mainPackagePath() string {
	if gf.VendorTool == "gomod" {
		return gf.Stager.BuildDir()
	}
	return filepath.Join(gf.GoPath, "src", gf.MainPackageName)
}

func (gf *Finalizer) goInstallLocation() string {
	return filepath.Join(gf.Stager.DepDir(), "go"+gf.GoVersion)
}

func (gf *Finalizer) updatePackagesForVendor(packages []string) []string {
	var newPackages []string

	for _, pkg := range packages {
		vendored, _ := libbuildpack.FileExists(filepath.Join(gf.mainPackagePath(), "vendor", pkg))
		if pkg == "." || !vendored {
			newPackages = append(newPackages, pkg)
		} else {
			newPackages = append(newPackages, filepath.Join(gf.MainPackageName, "vendor", pkg))
		}
	}

	return newPackages
}
