// Rule parser using PyPy. To build this you need PyPy installed, but the stock one
// that comes with Ubuntu will not work since it doesn't include shared libraries.
// We have a deb at https://s3-eu-west-1.amazonaws.com/please-build/pypy_4.0.0_amd64.deb
// which contains essentially the contents of a recent PyPy tarball.
// On OSX installing through Homebrew should be fine.
//
// The interface to PyPy is done through cgo and cffi. This means that we need to write very little
// actual C code; nearly all of it is in interpreter.h and is just declarations. What remains in
// interpreter.c is essentially just glue to handle limitations of cgo and the way we're using
// callbacks etc.
// The setup isn't actually extremely complex but some care is needed; it's relatively rare to need
// to modify it (generally only when adding new properties to build targets) but when you do you
// must make sure this file, interpreter.h and rules/please_parser.py all agree about struct
// definitions etc. Bad Things will happen if you do not.

package parse

import (
	"core"
	"crypto/sha1"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/op/go-logging"
)

/*
#cgo CFLAGS: --std=c99 -I/usr/include/pypy -Werror
#cgo LDFLAGS: -lpypy-c
#include "interpreter.h"
*/
import "C"

var log = logging.MustGetLogger("parse")

// Communicated back from PyPy to indicate that a parse has been deferred because
// we need to wait for another target to build.
const pyDeferParse = "_DEFER_"

var cDeferParse = C.CString(pyDeferParse)

// Callback state about how we communicate with the interpreter.
type PleaseCallbacks struct {
	ParseFile, ParseCode                                                                         *C.ParseFileCallback
	AddTarget, AddSrc, AddData, AddDep, AddExportedDep, AddTool, AddOut, AddVis, AddLabel        unsafe.Pointer
	AddHash, AddLicence, AddTestOutput, AddRequire, AddProvide, AddNamedSrc, SetContainerSetting unsafe.Pointer
	Glob, GetIncludeFile, GetSubincludeFile, GetLabels, SetPreBuildFunction                      unsafe.Pointer
	SetPostBuildFunction, AddDependency, AddOutput, AddLicencePost, SetCommand                   unsafe.Pointer
	SetConfigValue                                                                               *C.SetConfigValueCallback
	PreBuildCallbackRunner                                                                       *C.PreBuildCallbackRunner
	PostBuildCallbackRunner                                                                      *C.PostBuildCallbackRunner
	Log                                                                                          unsafe.Pointer
}

var callbacks PleaseCallbacks

// Something of a hack - we need to know these for globbing correctly but don't have
// access to the actual config object inside the glob function.
// Fortunately it doesn't change at runtime so we can stash these away...
var buildFileNames []string

// To ensure we only initialise once.
var initializeOnce sync.Once

// Code to initialise the Python interpreter.
func initializeInterpreter(config core.Configuration) {
	log.Debug("Initialising interpreter...")
	buildFileNames = config.Please.BuildFileName

	// PyPy becomes very unhappy if Go schedules it to a different OS thread during
	// its initialisation. Force it to stay on this one thread for now.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	C.rpython_startup_code()
	libpypy := locateLibPyPy(config)
	defer C.free(unsafe.Pointer(libpypy))
	if result := C.pypy_setup_home(libpypy, 1); result != 0 {
		log.Fatalf("Failed to initialise PyPy (error %d)\n", result)
	}
	C.pypy_init_threads()

	// Load interpreter & set up callbacks for communication
	log.Debug("Initialising interpreter environment...")
	data := loadAsset("please_parser.py")
	defer C.free(unsafe.Pointer(data))
	if result := C.InitialiseInterpreter(data, unsafe.Pointer(&callbacks)); result != 0 {
		panic(fmt.Sprintf("Failed to initialise parsing callbacks, error %d", result))
	}
	setConfigValue("PLZ_VERSION", config.Please.Version)
	setConfigValue("GO_VERSION", config.Go.Version)
	setConfigValue("GO_STRIP", config.Go.Strip)
	setConfigValue("PIP_TOOL", config.Python.PipTool)
	setConfigValue("PEX_TOOL", config.Python.PexTool)
	setConfigValue("DEFAULT_PYTHON_INTERPRETER", config.Python.DefaultInterpreter)
	setConfigValue("PYTHON_MODULE_DIR", config.Python.ModuleDir)
	setConfigValue("PYTHON_DEFAULT_PIP_REPO", config.Python.DefaultPipRepo)
	if config.Python.UsePyPI {
		setConfigValue("USE_PYPI", "true")
	} else {
		setConfigValue("USE_PYPI", "")
	}
	setConfigValue("JAVAC_TOOL", config.Java.JavacTool)
	setConfigValue("JAR_TOOL", config.Java.JarTool)
	setConfigValue("JARCAT_TOOL", config.Java.JarCatTool)
	setConfigValue("JUNIT_RUNNER", config.Java.JUnitRunner)
	setConfigValue("DEFAULT_TEST_PACKAGE", config.Java.DefaultTestPackage)
	setConfigValue("PLEASE_MAVEN_TOOL", config.Java.PleaseMavenTool)
	setConfigValue("JAVA_SOURCE_LEVEL", config.Java.SourceLevel)
	setConfigValue("JAVA_TARGET_LEVEL", config.Java.TargetLevel)
	setConfigValue("CC_TOOL", config.Cpp.CCTool)
	setConfigValue("LD_TOOL", config.Cpp.LdTool)
	setConfigValue("DEFAULT_CFLAGS", config.Cpp.DefaultCflags)
	setConfigValue("DEFAULT_TEST_CFLAGS", config.Cpp.DefaultTestCflags)
	setConfigValue("DEFAULT_LDFLAGS", config.Cpp.DefaultLdflags)
	setConfigValue("DEFAULT_TEST_LDFLAGS", config.Cpp.DefaultTestLdflags)
	setConfigValue("DEFAULT_NAMESPACE", config.Cpp.DefaultNamespace)
	setConfigValue("OS", runtime.GOOS)
	setConfigValue("ARCH", runtime.GOARCH)
	for _, language := range config.Proto.Language {
		setConfigValue("PROTO_LANGUAGES", language)
	}
	setConfigValue("PROTOC_TOOL", config.Proto.ProtocTool)
	setConfigValue("PROTOC_GO_PLUGIN", config.Proto.ProtocGoPlugin)
	setConfigValue("GRPC_PYTHON_PLUGIN", config.Proto.GrpcPythonPlugin)
	setConfigValue("GRPC_JAVA_PLUGIN", config.Proto.GrpcJavaPlugin)
	setConfigValue("PROTOC_VERSION", config.Proto.ProtocVersion)
	setConfigValue("PROTO_PYTHON_DEP", config.Proto.PythonDep)
	setConfigValue("PROTO_JAVA_DEP", config.Proto.JavaDep)
	setConfigValue("PROTO_GO_DEP", config.Proto.GoDep)
	setConfigValue("PROTO_PYTHON_PACKAGE", config.Proto.PythonPackage)
	setConfigValue("GRPC_VERSION", config.Proto.GrpcVersion)
	setConfigValue("GRPC_PYTHON_DEP", config.Proto.PythonGrpcDep)
	setConfigValue("GRPC_JAVA_DEP", config.Proto.JavaGrpcDep)
	setConfigValue("GRPC_GO_DEP", config.Proto.GoGrpcDep)

	// Load all the builtin rules
	log.Debug("Loading builtin build rules...")
	loadBuiltinRules("misc_rules.py")
	loadBuiltinRules("sh_rules.py")
	loadBuiltinRules("python_rules.py")
	loadBuiltinRules("java_rules.py")
	loadBuiltinRules("cc_rules.py")
	loadBuiltinRules("go_rules.py")
	loadBuiltinRules("proto_rules.py")
	log.Debug("Interpreter ready")
}

// locateLibPyPy returns a C string corresponding to the location of libpypy.
// It dies if it cannot be located successfully.
func locateLibPyPy(config core.Configuration) *C.char {
	// This is something of a hack to handle PyPy's dynamic location of itself.
	for _, location := range config.Please.PyPyLocation {
		if core.PathExists(location) {
			return C.CString(location)
		}
	}
	log.Fatalf("Cannot locate libpypy in any of [%s]\n", strings.Join(config.Please.PyPyLocation, ", "))
	return nil
}

func setConfigValue(name string, value string) {
	cName := C.CString(name)
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cValue))
	C.SetConfigValue(callbacks.SetConfigValue, cName, cValue)
}

func loadBuiltinRules(path string) {
	data := loadAsset(path)
	defer C.free(unsafe.Pointer(data))
	cPackageName := C.CString(path)
	defer C.free(unsafe.Pointer(cPackageName))
	if result := C.GoString(C.ParseFile(callbacks.ParseCode, data, cPackageName, nil)); result != "" {
		panic(fmt.Sprintf("Failed to interpret builtin build rules from %s: %s", path, result))
	}
}

func loadAsset(path string) *C.char {
	data, err := Asset(path)
	if err != nil {
		panic(fmt.Sprintf("Failed to load builtin build rules from %s", path))
	}
	// well this is pretty inefficient... we end up with three copies of the data for no
	// really good reason.
	return C.CString(string(data))
}

// parsePackageFile parses a single BUILD file.
// It returns true if parsing is deferred and waiting on other build actions, false otherwise on success
// and will panic on errors.
func parsePackageFile(state *core.BuildState, filename string, pkg *core.Package) bool {
	log.Debug("Parsing package file %s", filename)
	initializeOnce.Do(func() { initializeInterpreter(state.Config) })
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// TODO(pebers): It seems like we should be calling C.pypy_attach_thread here once per OS thread.
	//               That only seems to introduce problems though and not solve them; not sure if that is
	//               because we are doing thread-unsafe things in our parser, more go/c/pypy interface
	//               issues or something more mysterious. Regardless, it would be nice to understand
	//               more what's going on there and see if we can solve - I'm not sure we really have
	//               multithreaded parsing without it.
	cFilename := C.CString(filename)
	cPackageName := C.CString(pkg.Name)
	defer C.free(unsafe.Pointer(cFilename))
	defer C.free(unsafe.Pointer(cPackageName))
	if ret := C.GoString(C.ParseFile(callbacks.ParseFile, cFilename, cPackageName, unsafe.Pointer(pkg))); ret != "" && ret != pyDeferParse {
		panic(fmt.Sprintf("Failed to parse file %s: %s", filename, ret))
	} else {
		return ret == pyDeferParse
	}
}

//export AddTarget
func AddTarget(pkgPtr unsafe.Pointer, cName, cCmd, cTestCmd *C.char, binary bool, test bool,
	needsTransitiveDeps, outputIsComplete, containerise, noTestOutput, skipCache, testOnly bool,
	flakiness, buildTimeout, testTimeout int, cBuildingDescription *C.char) unsafe.Pointer {
	buildingDescription := ""
	if cBuildingDescription != nil {
		buildingDescription = C.GoString(cBuildingDescription)
	}
	return addTarget(pkgPtr, C.GoString(cName), C.GoString(cCmd), C.GoString(cTestCmd),
		binary, test, needsTransitiveDeps, outputIsComplete, containerise, noTestOutput,
		skipCache, testOnly, flakiness, buildTimeout, testTimeout, buildingDescription)
}

// addTarget adds a new build target to the graph.
// Separated from AddTarget to make it possible to test (since you can't mix cgo and go test).
func addTarget(pkgPtr unsafe.Pointer, name, cmd, testCmd string, binary bool, test bool,
	needsTransitiveDeps, outputIsComplete, containerise, noTestOutput, skipCache, testOnly bool,
	flakiness, buildTimeout, testTimeout int, buildingDescription string) unsafe.Pointer {
	pkg := (*core.Package)(pkgPtr)
	target := core.NewBuildTarget(core.NewBuildLabel(pkg.Name, name))
	target.IsBinary = binary
	target.IsTest = test
	target.NeedsTransitiveDependencies = needsTransitiveDeps
	target.OutputIsComplete = outputIsComplete
	target.Containerise = containerise
	target.NoTestOutput = noTestOutput
	target.SkipCache = skipCache
	target.TestOnly = testOnly
	target.Flakiness = flakiness
	target.BuildTimeout = buildTimeout
	target.TestTimeout = testTimeout
	if containerise {
		// Automatically label containerised tests.
		target.AddLabel("container")
	}
	if buildingDescription != "" {
		target.BuildingDescription = buildingDescription
	}
	if binary {
		target.AddLabel("bin")
	}
	target.Command = cmd
	target.TestCommand = testCmd
	if _, present := pkg.Targets[name]; present {
		panic(fmt.Sprintf("Duplicate build target in %s: %s", pkg.Name, name))
	}
	if target.TestCommand != "" && !target.IsTest {
		panic(fmt.Sprintf("Target %s has been given a test command but isn't a test", target.Label))
	} else if target.IsTest && target.TestCommand == "" {
		panic(fmt.Sprintf("Target %s is a test but hasn't been given a test command", target.Label))
	}
	pkg.Targets[name] = target
	if core.State.Graph.Package(pkg.Name) != nil {
		// Package already added, so we're probably in a post-build function. Add target directly to graph now.
		log.Debug("Adding new target %s directly to graph", target.Label)
		core.State.Graph.AddTarget(target)
	}
	return unsafe.Pointer(target)
}

//export SetPreBuildFunction
func SetPreBuildFunction(callback uintptr, cBytecode *C.char, cTarget unsafe.Pointer) {
	target := (*core.BuildTarget)(cTarget)
	target.PreBuildFunction = callback
	hash := sha1.Sum([]byte(C.GoString(cBytecode)))
	target.PreBuildHash = hash[:]
}

//export SetPostBuildFunction
func SetPostBuildFunction(callback uintptr, cBytecode *C.char, cTarget unsafe.Pointer) {
	target := (*core.BuildTarget)(cTarget)
	target.PostBuildFunction = callback
	hash := sha1.Sum([]byte(C.GoString(cBytecode)))
	target.PostBuildHash = hash[:]
}

//export AddDependency
func AddDependency(cPackage unsafe.Pointer, cTarget *C.char, cDep *C.char, exported bool) {
	target := getTargetPost(cPackage, cTarget)
	dep, _ := core.ParseBuildFileLabel(C.GoString(cDep), target.Label.PackageName)
	target.AddDependency(dep)
	if exported {
		target.AddExportedDependency(dep)
	}
	core.State.Graph.AddDependency(target.Label, dep)
}

//export AddOutputPost
func AddOutputPost(cPackage unsafe.Pointer, cTarget *C.char, cOut *C.char) {
	target := getTargetPost(cPackage, cTarget)
	out := C.GoString(cOut)
	pkg := (*core.Package)(cPackage)
	pkg.RegisterOutput(out, target)
	target.AddOutput(out)
}

//export AddLicencePost
func AddLicencePost(cPackage unsafe.Pointer, cTarget *C.char, cLicence *C.char) {
	target := getTargetPost(cPackage, cTarget)
	target.AddLicence(C.GoString(cLicence))
}

//export SetCommand
func SetCommand(cPackage unsafe.Pointer, cTarget *C.char, cCommand *C.char) {
	target := getTargetPost(cPackage, cTarget)
	oldCommand := target.Command
	target.Command = C.GoString(cCommand)
	log.Debug("Set command for %s to %s (was %s)", target.Label, target.Command, oldCommand)
	// It'd be nice if we could ensure here that we're in the pre-build function
	// but not the post-build function which is too late to have any effect.
	// OTOH while it's ineffective it shouldn't cause any trouble trying it either...
}

// Called by above to get a target from the current package.
// Panics if the target is not in the current package or has already been built.
func getTargetPost(cPackage unsafe.Pointer, cTarget *C.char) *core.BuildTarget {
	pkg := (*core.Package)(cPackage)
	name := C.GoString(cTarget)
	target, present := pkg.Targets[name]
	if !present {
		panic(fmt.Sprintf("Unknown build target %s in %s", name, pkg.Name))
	}
	// It'd be cheating to try to modify targets that're already built.
	// Prohibit this because it'd likely end up with nasty race conditions.
	if target.State() >= core.Built {
		panic(fmt.Sprintf("Attempted to modify target %s, but it's already built", target.Label))
	}
	return target
}

//export AddSource
func AddSource(cTarget unsafe.Pointer, cSource *C.char) {
	target := (*core.BuildTarget)(cTarget)
	source := parseSource(C.GoString(cSource), target.Label.PackageName)
	target.Sources = append(target.Sources, source)
	if label := source.Label(); label != nil {
		target.AddDependency(*label)
	}
}

// Parses an incoming source label as either a file or a build label.
// Identifies if the file is owned by this package and dies if not.
func parseSource(src string, packageName string) core.BuildInput {
	if core.LooksLikeABuildLabel(src) {
		label, file := core.ParseBuildFileLabel(src, packageName)
		if file != "" {
			return core.BuildFileLabel{BuildLabel: label, File: file}
		}
		return label
	} else if strings.Contains(src, "../") {
		panic(fmt.Errorf("'%s' (in package %s) is an invalid path; build target paths can't contain ../", src, packageName))
	} else if src[0] == '/' {
		panic(fmt.Errorf("'%s' (in package %s) is an absolute path; that's not allowed.", src, packageName))
	} else if strings.Contains(src, "/") {
		// Target is in a subdirectory, check nobody else owns that.
		for dir := path.Dir(path.Join(packageName, src)); dir != packageName && dir != "."; dir = path.Dir(dir) {
			if isPackage(dir) {
				panic(fmt.Errorf("Package %s tries to use file %s, but that belongs to another package (%s).", packageName, src, dir))
			}
		}
	}
	return core.FileLabel{File: src, Package: packageName}
}

//export AddNamedSource
func AddNamedSource(cTarget unsafe.Pointer, cName *C.char, cSource *C.char) {
	target := (*core.BuildTarget)(cTarget)
	source := parseSource(C.GoString(cSource), target.Label.PackageName)
	target.AddNamedSource(C.GoString(cName), source)
	if label := source.Label(); label != nil {
		target.AddDependency(*label)
	}
}

//export AddData
func AddData(cTarget unsafe.Pointer, cData *C.char) {
	target := (*core.BuildTarget)(cTarget)
	data := parseSource(C.GoString(cData), target.Label.PackageName)
	target.Data = append(target.Data, data)
	if label := data.Label(); label != nil {
		target.AddDependency(*label)
	}
}

//export AddOutput
func AddOutput(cTarget unsafe.Pointer, cOutput *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.AddOutput(C.GoString(cOutput))
}

//export AddDep
func AddDep(cTarget unsafe.Pointer, cDep *C.char) {
	target := (*core.BuildTarget)(cTarget)
	dep, _ := core.ParseBuildFileLabel(C.GoString(cDep), target.Label.PackageName)
	target.AddDependency(dep)
}

//export AddExportedDep
func AddExportedDep(cTarget unsafe.Pointer, cDep *C.char) {
	target := (*core.BuildTarget)(cTarget)
	dep, _ := core.ParseBuildFileLabel(C.GoString(cDep), target.Label.PackageName)
	target.AddDependency(dep)
	target.AddExportedDependency(dep)
}

//export AddTool
func AddTool(cTarget unsafe.Pointer, cTool *C.char) {
	target := (*core.BuildTarget)(cTarget)
	tool, _ := core.ParseBuildFileLabel(C.GoString(cTool), target.Label.PackageName)
	target.Tools = append(target.Tools, tool)
	target.AddDependency(tool)
}

//export AddVis
func AddVis(cTarget unsafe.Pointer, cVis *C.char) {
	target := (*core.BuildTarget)(cTarget)
	vis := C.GoString(cVis)
	if vis == "PUBLIC" {
		target.Visibility = append(target.Visibility, core.NewBuildLabel("", "..."))
	} else {
		target.Visibility = append(target.Visibility, core.ParseBuildLabel(vis, target.Label.PackageName))
	}
}

//export AddLabel
func AddLabel(cTarget unsafe.Pointer, cLabel *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.AddLabel(C.GoString(cLabel))
}

//export AddHash
func AddHash(cTarget unsafe.Pointer, cHash *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.Hashes = append(target.Hashes, C.GoString(cHash))
}

//export AddLicence
func AddLicence(cTarget unsafe.Pointer, cLicence *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.AddLicence(C.GoString(cLicence))
}

//export AddTestOutput
func AddTestOutput(cTarget unsafe.Pointer, cTestOutput *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.TestOutputs = append(target.TestOutputs, C.GoString(cTestOutput))
}

//export AddRequire
func AddRequire(cTarget unsafe.Pointer, cRequire *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.Requires = append(target.Requires, C.GoString(cRequire))
	// Requirements are also implicit labels
	target.AddLabel(C.GoString(cRequire))
}

//export AddProvide
func AddProvide(cTarget unsafe.Pointer, cLanguage *C.char, cDep *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.AddProvide(C.GoString(cLanguage), core.ParseBuildLabel(C.GoString(cDep), target.Label.PackageName))
}

//export SetContainerSetting
func SetContainerSetting(cTarget unsafe.Pointer, cName, cValue *C.char) {
	target := (*core.BuildTarget)(cTarget)
	target.SetContainerSetting(strings.Replace(C.GoString(cName), "_", "", -1), C.GoString(cValue))
}

//export GetIncludeFile
func GetIncludeFile(cPackage unsafe.Pointer, cLabel *C.char) *C.char {
	pkg := (*core.Package)(cPackage)
	label := C.GoString(cLabel)
	if !strings.HasPrefix(label, "//") {
		panic("include_defs argument must be an absolute path (ie. start with //)")
	}
	relPath := strings.TrimLeft(label, "/")
	pkg.RegisterSubinclude(relPath)
	return C.CString(path.Join(core.RepoRoot, relPath))
}

// GetSubincludeFile is a callback to the interpreter that returns the path it
// should be opening in order to subinclude() a build target.
// For convenience we use in-band signalling for some errors since C can't handle multiple return values :)
// Fatal errors (like incorrect build labels etc) will cause a panic.
//export GetSubincludeFile
func GetSubincludeFile(cPackage unsafe.Pointer, cLabel *C.char) *C.char {
	pkg := (*core.Package)(cPackage)
	label := core.ParseBuildLabel(C.GoString(cLabel), pkg.Name)
	pkgLabel := core.BuildLabel{PackageName: pkg.Name, Name: "all"}
	target := core.State.Graph.Target(label)
	if target == nil {
		// Might not have been parsed yet. Check for that first.
		if subincludePackage := core.State.Graph.Package(label.PackageName); subincludePackage == nil {
			deferParse(label, pkg)
			return cDeferParse // Not an error, they'll just have to wait.
		}
		panic(fmt.Sprintf("Failed to subinclude %s; package %s has no target by that name", label, label.PackageName))
	} else if tmp := core.NewBuildTarget(pkgLabel); !tmp.CanSee(target) {
		panic(fmt.Sprintf("Can't subinclude %s from %s due to visibility constraints", label, pkg.Name))
	} else if len(target.Outputs()) != 1 {
		panic(fmt.Sprintf("Can't subinclude %s, subinclude targets must have exactly one output", label))
	} else if target.State() < core.Built {
		deferParse(label, pkg)
		return cDeferParse // Again, they'll have to wait for this guy to build.
	}
	// Well if we made it to here it's actually ready to go, so tell them where to get it.
	return C.CString(path.Join(target.OutDir(), target.Outputs()[0]))
}

// runPreBuildFunction runs the pre-build function for a single target.
func runPreBuildFunction(pkg *core.Package, target *core.BuildTarget) error {
	cName := C.CString(target.Label.Name)
	defer C.free(unsafe.Pointer(cName))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if result := C.GoString(C.RunPreBuildFunction(callbacks.PreBuildCallbackRunner, C.size_t(target.PreBuildFunction), unsafe.Pointer(pkg), cName)); result != "" {
		return fmt.Errorf("Failed to run pre-build function for target %s: %s", target.Label.String(), result)
	}
	return nil
}

// runPostBuildFunction runs the post-build function for a single target.
func runPostBuildFunction(pkg *core.Package, target *core.BuildTarget, out string) error {
	cName := C.CString(target.Label.Name)
	cOutput := C.CString(out)
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cOutput))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if result := C.GoString(C.RunPostBuildFunction(callbacks.PostBuildCallbackRunner, C.size_t(target.PostBuildFunction), unsafe.Pointer(pkg), cName, cOutput)); result != "" {
		return fmt.Errorf("Failed to run post-build function for target %s: %s", target.Label.String(), result)
	}
	return nil
}

// Unfortunately there doesn't seem to be any API to do this dynamically :(
var logLevelFuncs = map[logging.Level]func(format string, args ...interface{}){
	logging.CRITICAL: log.Fatalf,
	logging.ERROR:    log.Error,
	logging.WARNING:  log.Warning,
	logging.NOTICE:   log.Notice,
	logging.INFO:     log.Info,
	logging.DEBUG:    log.Debug,
}

//export Log
func Log(level int, cPackage unsafe.Pointer, cMessage *C.char) {
	pkg := (*core.Package)(cPackage)
	f, present := logLevelFuncs[logging.Level(level)]
	if !present {
		f = log.Error
	}
	f("//%s/BUILD: %s", pkg.Name, C.GoString(cMessage))
}

//export Glob
func Glob(cPackage *C.char, cIncludes **C.char, numIncludes int, cExcludes **C.char, numExcludes int, includeHidden bool) **C.char {
	packageName := C.GoString(cPackage)
	filenames := []string{}
	for i := 0; i < numIncludes; i++ {
		matches, err := glob(packageName, C.GoString(C.getStringFromArray(cIncludes, C.int(i))))
		if err != nil {
			panic(err)
		}
		for _, filename := range matches {
			if !includeHidden {
				// Exclude hidden & temporary files
				_, file := path.Split(filename)
				if strings.HasPrefix(file, ".") || (strings.HasPrefix(file, "#") && strings.HasSuffix(file, "#")) {
					continue
				}
			}
			if !shouldExcludeMatch(filename, packageName, cExcludes, numExcludes) {
				if strings.HasPrefix(filename, packageName) {
					filename = filename[len(packageName)+1:] // +1 to strip the slash too
				}
				filenames = append(filenames, filename)
			}
		}
	}
	return stringSliceToCStringArray(filenames)
}

// stringSliceToCDoubleArray converts a Go slice of strings to a C array of char*'s.
// The returned array is terminated by a null pointer - the Python interpreter code will
// understand how to turn this back into Python strings.
func stringSliceToCStringArray(s []string) **C.char {
	ret := C.allocateStringArray(C.int(len(s) + 1))
	for i, x := range s {
		C.setStringInArray(ret, C.int(i), C.CString(x))
	}
	C.setStringInArray(ret, C.int(len(s)), nil)
	return ret
}

func shouldExcludeMatch(match string, packageName string, cExcludes **C.char, numExcludes int) bool {
	for j := 0; j < numExcludes; j++ {
		exclPattern := path.Join(packageName, C.GoString(C.getStringFromArray(cExcludes, C.int(j))))
		matches, err := filepath.Match(exclPattern, match)
		if err != nil {
			panic(err)
		} else if matches {
			return true
		}
	}
	return false
}

func glob(rootPath, pattern string) ([]string, error) {
	// Go's Glob function doesn't handle Ant-style ** patterns. Do it ourselves if we have to,
	// but we prefer not since our solution will have to do a potentially inefficient walk.
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(path.Join(rootPath, pattern))
	}

	matches := []string{}
	// Turn the pattern into a regex. Oh dear...
	pattern = strings.Replace(pattern, "*", "[^/]*", -1)        // handle single (all) * components
	pattern = strings.Replace(pattern, "[^/]*[^/]*", ".*", -1)  // handle ** components
	pattern = strings.Replace(pattern, "/.*/", "/(?:.*/)?", -1) // allow /**/ to match nothing
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return matches, err
	}

	err = filepath.Walk(rootPath, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		} else if info.IsDir() && name != rootPath && isPackage(name) {
			return filepath.SkipDir // Can't glob past a package boundary
		} else if !info.IsDir() && regex.MatchString(name) {
			matches = append(matches, name)
		}
		return nil
	})
	return matches, err
}

// Memoize this to cut down on filesystem operations
var isPackageMemo = map[string]bool{}
var isPackageMutex sync.Mutex

func isPackage(name string) bool {
	isPackageMutex.Lock()
	defer isPackageMutex.Unlock()
	if ret, present := isPackageMemo[name]; present {
		return ret
	}
	ret := isPackageInternal(name)
	isPackageMemo[name] = ret
	return ret
}

func isPackageInternal(name string) bool {
	for _, buildFileName := range buildFileNames {
		if core.FileExists(path.Join(name, buildFileName)) {
			return true
		}
	}
	return false
}

//export GetLabels
func GetLabels(cPackage unsafe.Pointer, cTarget *C.char, cPrefix *C.char) **C.char {
	target := getTargetPost(cPackage, cTarget)
	prefix := C.GoString(cPrefix)
	if target.State() != core.Building {
		log.Fatalf("get_labels called for %s incorrectly; the only time this is safe to call is from its own pre-build function.", target.Label)
	}
	labels := map[string]bool{}
	var getLabels func(*core.BuildTarget)
	getLabels = func(target *core.BuildTarget) {
		for _, label := range target.Labels {
			if strings.HasPrefix(label, prefix) {
				labels[strings.TrimSpace(strings.TrimPrefix(label, prefix))] = true
			}
		}
		for _, dep := range target.Dependencies {
			getLabels(dep)
		}
	}
	getLabels(target)
	ret := make([]string, len(labels))
	i := 0
	for label := range labels {
		ret[i] = label
		i++
	}
	sort.Strings(ret)
	return stringSliceToCStringArray(ret)
}