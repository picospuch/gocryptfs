package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/syslog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"

	"github.com/rfjakob/gocryptfs/internal/configfile"
	"github.com/rfjakob/gocryptfs/internal/contentenc"
	"github.com/rfjakob/gocryptfs/internal/cryptocore"
	"github.com/rfjakob/gocryptfs/internal/fusefrontend"
	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/toggledlog"
)

const (
	// Exit codes
	ERREXIT_USAGE      = 1
	ERREXIT_MOUNT      = 3
	ERREXIT_CIPHERDIR  = 6
	ERREXIT_INIT       = 7
	ERREXIT_LOADCONF   = 8
	ERREXIT_PASSWORD   = 9
	ERREXIT_MOUNTPOINT = 10
)

type argContainer struct {
	debug, init, zerokey, fusedebug, openssl, passwd, foreground, version,
	plaintextnames, quiet, diriv, emenames, gcmiv128, nosyslog, wpanic bool
	masterkey, mountpoint, cipherdir, cpuprofile, config, extpass,
	memprofile string
	notifypid, scryptn int
}

var flagSet *flag.FlagSet

// GitVersion will be set by the build script "build.bash"
var GitVersion = "[version not set - please compile using ./build.bash]"

func initDir(args *argContainer) {
	err := checkDirEmpty(args.cipherdir)
	if err != nil {
		fmt.Printf("Invalid cipherdir: %v\n", err)
		os.Exit(ERREXIT_INIT)
	}

	// Create gocryptfs.conf
	toggledlog.Info.Printf("Choose a password for protecting your files.")
	password := readPasswordTwice(args.extpass)
	err = configfile.CreateConfFile(args.config, password, args.plaintextnames, args.scryptn)
	if err != nil {
		fmt.Println(err)
		os.Exit(ERREXIT_INIT)
	}

	if args.diriv && !args.plaintextnames {
		// Create gocryptfs.diriv in the root dir
		err = nametransform.WriteDirIV(args.cipherdir)
		if err != nil {
			fmt.Println(err)
			os.Exit(ERREXIT_INIT)
		}
	}

	toggledlog.Info.Printf(colorGreen + "The filesystem has been created successfully." + colorReset)
	toggledlog.Info.Printf(colorGrey+"You can now mount it using: %s %s MOUNTPOINT"+colorReset,
		toggledlog.ProgramName, args.cipherdir)
	os.Exit(0)
}

func usageText() {
	printVersion()
	fmt.Printf("\n")
	fmt.Printf("Usage: %s -init|-passwd [OPTIONS] CIPHERDIR\n", toggledlog.ProgramName)
	fmt.Printf("  or   %s [OPTIONS] CIPHERDIR MOUNTPOINT\n", toggledlog.ProgramName)
	fmt.Printf("\nOptions:\n")
	flagSet.PrintDefaults()
}

// loadConfig - load the config file "filename", prompting the user for the password
func loadConfig(args *argContainer) (masterkey []byte, confFile *configfile.ConfFile) {
	// Check if the file exists at all before prompting for a password
	_, err := os.Stat(args.config)
	if err != nil {
		fmt.Println(err)
		os.Exit(ERREXIT_LOADCONF)
	}
	if args.extpass == "" {
		fmt.Printf("Password: ")
	}
	pw := readPassword(args.extpass)
	toggledlog.Info.Printf("Decrypting master key... ")
	toggledlog.Warn.Enabled = false // Silence DecryptBlock() error messages on incorrect password
	masterkey, confFile, err = configfile.LoadConfFile(args.config, pw)
	toggledlog.Warn.Enabled = true
	if err != nil {
		fmt.Println(err)
		fmt.Println(colorRed + "Wrong password." + colorReset)
		os.Exit(ERREXIT_LOADCONF)
	}
	toggledlog.Info.Printf("done.")

	return masterkey, confFile
}

// changePassword - change the password of config file "filename"
func changePassword(args *argContainer) {
	masterkey, confFile := loadConfig(args)
	fmt.Println("Please enter your new password.")
	newPw := readPasswordTwice(args.extpass)
	confFile.EncryptKey(masterkey, newPw, confFile.ScryptObject.LogN())
	err := confFile.WriteFile()
	if err != nil {
		fmt.Println(err)
		os.Exit(ERREXIT_INIT)
	}
	toggledlog.Info.Printf("Password changed.")
	os.Exit(0)
}

// printVersion - print a version string like
// "gocryptfs v0.3.1-31-g6736212-dirty; on-disk format 2"
func printVersion() {
	fmt.Printf("%s %s; on-disk format %d\n", toggledlog.ProgramName, GitVersion, contentenc.CurrentVersion)
}

func main() {
	runtime.GOMAXPROCS(4)
	var err error
	var args argContainer
	setupColors()

	// Parse command line arguments
	flagSet = flag.NewFlagSet(toggledlog.ProgramName, flag.ExitOnError)
	flagSet.Usage = usageText
	flagSet.BoolVar(&args.debug, "d", false, "")
	flagSet.BoolVar(&args.debug, "debug", false, "Enable debug output")
	flagSet.BoolVar(&args.fusedebug, "fusedebug", false, "Enable fuse library debug output")
	flagSet.BoolVar(&args.init, "init", false, "Initialize encrypted directory")
	flagSet.BoolVar(&args.zerokey, "zerokey", false, "Use all-zero dummy master key")
	flagSet.BoolVar(&args.openssl, "openssl", true, "Use OpenSSL instead of built-in Go crypto")
	flagSet.BoolVar(&args.passwd, "passwd", false, "Change password")
	flagSet.BoolVar(&args.foreground, "f", false, "Stay in the foreground")
	flagSet.BoolVar(&args.version, "version", false, "Print version and exit")
	flagSet.BoolVar(&args.plaintextnames, "plaintextnames", false, "Do not encrypt file names")
	flagSet.BoolVar(&args.quiet, "q", false, "")
	flagSet.BoolVar(&args.quiet, "quiet", false, "Quiet - silence informational messages")
	flagSet.BoolVar(&args.diriv, "diriv", true, "Use per-directory file name IV")
	flagSet.BoolVar(&args.emenames, "emenames", true, "Use EME filename encryption. This option implies diriv.")
	flagSet.BoolVar(&args.gcmiv128, "gcmiv128", true, "Use an 128-bit IV for GCM encryption instead of Go's default of 96 bits")
	flagSet.BoolVar(&args.nosyslog, "nosyslog", false, "Do not redirect output to syslog when running in the background")
	flagSet.BoolVar(&args.wpanic, "wpanic", false, "When encountering a warning, panic and exit immediately")
	flagSet.StringVar(&args.masterkey, "masterkey", "", "Mount with explicit master key")
	flagSet.StringVar(&args.cpuprofile, "cpuprofile", "", "Write cpu profile to specified file")
	flagSet.StringVar(&args.memprofile, "memprofile", "", "Write memory profile to specified file")
	flagSet.StringVar(&args.config, "config", "", "Use specified config file instead of CIPHERDIR/gocryptfs.conf")
	flagSet.StringVar(&args.extpass, "extpass", "", "Use external program for the password prompt")
	flagSet.IntVar(&args.notifypid, "notifypid", 0, "Send USR1 to the specified process after "+
		"successful mount - used internally for daemonization")
	flagSet.IntVar(&args.scryptn, "scryptn", configfile.ScryptDefaultLogN, "scrypt cost parameter logN. "+
		"Setting this to a lower value speeds up mounting but makes the password susceptible to brute-force attacks")
	flagSet.Parse(os.Args[1:])

	// Fork a child into the background if "-f" is not set AND we are mounting a filesystem
	if !args.foreground && flagSet.NArg() == 2 {
		forkChild() // does not return
	}
	// "-v"
	if args.version {
		printVersion()
		os.Exit(0)
	}
	if args.debug {
		toggledlog.Debug.Enabled = true
		toggledlog.Debug.Printf("Debug output enabled")
	}
	if args.wpanic {
		toggledlog.Warn.PanicAfter = true
		toggledlog.Debug.Printf("Panicing on warnings")
	}
	// Every operation below requires CIPHERDIR. Check that we have it.
	if flagSet.NArg() >= 1 {
		args.cipherdir, _ = filepath.Abs(flagSet.Arg(0))
		err := checkDir(args.cipherdir)
		if err != nil {
			fmt.Printf(colorRed+"Invalid cipherdir: %v\n"+colorReset, err)
			os.Exit(ERREXIT_CIPHERDIR)
		}
	} else {
		usageText()
		os.Exit(ERREXIT_USAGE)
	}
	// "-q"
	if args.quiet {
		toggledlog.Info.Enabled = false
	}
	// "-config"
	if args.config != "" {
		args.config, err = filepath.Abs(args.config)
		if err != nil {
			fmt.Printf(colorRed+"Invalid \"-config\" setting: %v\n"+colorReset, err)
		}
		toggledlog.Info.Printf("Using config file at custom location %s", args.config)
	} else {
		args.config = filepath.Join(args.cipherdir, configfile.ConfDefaultName)
	}
	// "-cpuprofile"
	if args.cpuprofile != "" {
		toggledlog.Info.Printf("Writing CPU profile to %s", args.cpuprofile)
		f, err := os.Create(args.cpuprofile)
		if err != nil {
			fmt.Println(err)
			os.Exit(ERREXIT_INIT)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	// "-memprofile"
	if args.memprofile != "" {
		toggledlog.Info.Printf("Writing mem profile to %s", args.memprofile)
		f, err := os.Create(args.memprofile)
		if err != nil {
			fmt.Println(err)
			os.Exit(ERREXIT_INIT)
		}
		defer func() {
			pprof.WriteHeapProfile(f)
			f.Close()
			return
		}()
	}
	if args.cpuprofile != "" || args.memprofile != "" {
		fmt.Printf("Note: You must unmount gracefully, otherwise the profile file(s) will stay empty!\n")
	}
	// "-openssl"
	if args.openssl == false {
		toggledlog.Info.Printf("Openssl disabled")
	}
	// Operation flags: init, passwd or mount
	// "-init"
	if args.init {
		if flagSet.NArg() > 1 {
			fmt.Printf("Usage: %s -init [OPTIONS] CIPHERDIR\n", toggledlog.ProgramName)
			os.Exit(ERREXIT_USAGE)
		}
		initDir(&args) // does not return
	}
	// "-passwd"
	if args.passwd {
		if flagSet.NArg() > 1 {
			fmt.Printf("Usage: %s -passwd [OPTIONS] CIPHERDIR\n", toggledlog.ProgramName)
			os.Exit(ERREXIT_USAGE)
		}
		changePassword(&args) // does not return
	}
	// Mount
	// Check mountpoint
	if flagSet.NArg() != 2 {
		usageText()
		os.Exit(ERREXIT_USAGE)
	}
	args.mountpoint, err = filepath.Abs(flagSet.Arg(1))
	if err != nil {
		fmt.Printf(colorRed+"Invalid mountpoint: %v\n"+colorReset, err)
		os.Exit(ERREXIT_MOUNTPOINT)
	}
	err = checkDirEmpty(args.mountpoint)
	if err != nil {
		fmt.Printf(colorRed+"Invalid mountpoint: %v\n"+colorReset, err)
		os.Exit(ERREXIT_MOUNTPOINT)
	}
	// Get master key
	var masterkey []byte
	var confFile *configfile.ConfFile
	if args.masterkey != "" {
		// "-masterkey"
		toggledlog.Info.Printf("Using explicit master key.")
		masterkey = parseMasterKey(args.masterkey)
		toggledlog.Info.Printf("THE MASTER KEY IS VISIBLE VIA \"ps -auxwww\", ONLY USE THIS MODE FOR EMERGENCIES.")
	} else if args.zerokey {
		// "-zerokey"
		toggledlog.Info.Printf("Using all-zero dummy master key.")
		toggledlog.Info.Printf("ZEROKEY MODE PROVIDES NO SECURITY AT ALL AND SHOULD ONLY BE USED FOR TESTING.")
		masterkey = make([]byte, cryptocore.KeyLen)
	} else {
		// Load master key from config file
		masterkey, confFile = loadConfig(&args)
		printMasterKey(masterkey)
	}
	// Initialize FUSE server
	toggledlog.Debug.Printf("cli args: %v", args)
	srv := initFuseFrontend(masterkey, args, confFile)
	toggledlog.Info.Println(colorGreen + "Filesystem mounted and ready." + colorReset)
	// We are ready - send USR1 signal to our parent and switch to syslog
	if args.notifypid > 0 {
		sendUsr1(args.notifypid)

		if !args.nosyslog {
			toggledlog.Info.SwitchToSyslog(syslog.LOG_USER | syslog.LOG_INFO)
			toggledlog.Debug.SwitchToSyslog(syslog.LOG_USER | syslog.LOG_DEBUG)
			toggledlog.Warn.SwitchToSyslog(syslog.LOG_USER | syslog.LOG_WARNING)
		}
	}
	// Wait for SIGINT in the background and unmount ourselves if we get it.
	// This prevents a dangling "Transport endpoint is not connected" mountpoint.
	handleSigint(srv, args.mountpoint)
	// Jump into server loop. Returns when it gets an umount request from the kernel.
	srv.Serve()
	// main exits with code 0
}

// initFuseFrontend - initialize gocryptfs/fusefrontend
// Calls os.Exit on errors
func initFuseFrontend(key []byte, args argContainer, confFile *configfile.ConfFile) *fuse.Server {

	// Reconciliate CLI and config file arguments into a Args struct that is passed to the
	// filesystem implementation
	frontendArgs := fusefrontend.Args{
		Cipherdir:      args.cipherdir,
		Masterkey:      key,
		OpenSSL:        args.openssl,
		PlaintextNames: args.plaintextnames,
		DirIV:          args.diriv,
		EMENames:       args.emenames,
		GCMIV128:       args.gcmiv128,
	}
	// confFile is nil when "-zerokey" or "-masterkey" was used
	if confFile != nil {
		// Settings from the config file override command line args
		frontendArgs.PlaintextNames = confFile.IsFeatureFlagSet(configfile.FlagPlaintextNames)
		frontendArgs.DirIV = confFile.IsFeatureFlagSet(configfile.FlagDirIV)
		frontendArgs.EMENames = confFile.IsFeatureFlagSet(configfile.FlagEMENames)
		frontendArgs.GCMIV128 = confFile.IsFeatureFlagSet(configfile.FlagGCMIV128)
	}
	// EMENames implies DirIV, both on the command line and in the config file.
	if frontendArgs.EMENames {
		frontendArgs.DirIV = true
	}
	// PlainTexnames disables both EMENames and DirIV
	if frontendArgs.PlaintextNames {
		frontendArgs.DirIV = false
		frontendArgs.EMENames = false
	}
	jsonBytes, _ := json.MarshalIndent(frontendArgs, "", "\t")
	toggledlog.Debug.Printf("frontendArgs: %s", string(jsonBytes))

	finalFs := fusefrontend.NewFS(frontendArgs)
	pathFsOpts := &pathfs.PathNodeFsOptions{ClientInodes: true}
	pathFs := pathfs.NewPathNodeFs(finalFs, pathFsOpts)
	fuseOpts := &nodefs.Options{
		// These options are to be compatible with libfuse defaults,
		// making benchmarking easier.
		NegativeTimeout: time.Second,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
	}
	conn := nodefs.NewFileSystemConnector(pathFs.Root(), fuseOpts)
	var mOpts fuse.MountOptions
	mOpts.AllowOther = false
	// Set values shown in "df -T" and friends
	// First column, "Filesystem"
	mOpts.Options = append(mOpts.Options, "fsname="+args.cipherdir)
	// Second column, "Type", will be shown as "fuse." + Name
	mOpts.Name = "gocryptfs"

	srv, err := fuse.NewServer(conn.RawFS(), args.mountpoint, &mOpts)
	if err != nil {
		fmt.Printf("Mount failed: %v", err)
		os.Exit(ERREXIT_MOUNT)
	}
	srv.SetDebug(args.fusedebug)

	// All FUSE file and directory create calls carry explicit permission
	// information. We need an unrestricted umask to create the files and
	// directories with the requested permissions.
	syscall.Umask(0000)

	return srv
}

func handleSigint(srv *fuse.Server, mountpoint string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		err := srv.Unmount()
		if err != nil {
			fmt.Print(err)
			toggledlog.Info.Printf("Trying lazy unmount")
			cmd := exec.Command("fusermount", "-u", "-z", mountpoint)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
		}
		os.Exit(1)
	}()
}

// Escape sequences for terminal colors
var colorReset, colorGrey, colorRed, colorGreen string

func setupColors() {
	if terminal.IsTerminal(int(os.Stdout.Fd())) {
		colorReset = "\033[0m"
		colorGrey = "\033[2m"
		colorRed = "\033[31m"
		colorGreen = "\033[32m"
	}
}
