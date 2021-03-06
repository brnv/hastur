package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/docopt/docopt-go"
)

const containerSuffix = ".hastur"

const defaultPackages = `bash,coreutils,iproute2,iputils`

const usage = `hastur the unspeakable - zero-conf systemd container manager.

hastur is a simple wrapper around systemd-nspawn, that will start container
with overlayfs, pre-installed packages and bridged network available out
of the box.

hastur operates over specified root directory, which will hold base FS
for containers and numerous amount of containers derived from base FS.

Primary use of hastur is testing purposes, running testcases for distributed
services and running local network of trusted containers.

Usage:
    hastur -h | --help
    hastur [options] [-b=] [-s=] [-a=] [-p <packages>...] [-n=]
                     -S [--] [<command>...]
    hastur [options] -Q (-i | -c)
    hastur [options] -Q (--rootfs|--ip) <name>
    hastur [options] -D [-f] <name>
    hastur [options] --free

Options:
    -h --help        Show this help.
    -r <root>        Root directory which will hold containers.
                      [default: /var/lib/hastur/]
    -q               Be quiet. Do not report status messages from nspawn.
    -f               Force operation.

Create options:
    -S               Create and start container.
       <command>     Execute specified command in created
                      container.
      -b <bridge>    Bridge interface name and, optionally, an address,
                      separated by colon.
                      If bridge does not exists, it will be automatically
                      created.
                      [default: br0:10.0.0.1/8]
      -t <iface>     Use host network and gain access to external network.
                      Interface will pair given interface with bridge.
      -s <storage>   Use specified storageSpec backend for container base
                      images and containers themselves. By default, overlayfs
                      will be used to provide COW for base system. If overlayfs
                      is not possible on current FS and no other storageSpec
                      engine is possible, tmpfs will be mounted in specified
                      root dir to provide groundwork for overlayfs.
                      [default: autodetect]
         <storage>   Possible values are:
                      * autodetect - use one of available storageSpec engines
                      depending on current FS.
                      * overlayfs:N - use current FS and overlayfs on top;
                      if overlayfs is unsupported on current FS, mount tmpfs of
                      size N first.
      -p <packages>  Packages to install, separated by comma.
                      [default: ` + defaultPackages + `]
      -n <name>      Use specified container name. If not specified, randomly
                      generated name will be used and container will be
                      considered ephemeral, e.g. will be destroyed on <command>
                      exit.
      -a <address>   Use specified IP address/netmask. If not specified,
                      automatically generated adress from 10.0.0.0/8 will
                      be used.
      -k             Keep container after exit if it name was autogenerated.
      -x <dir>       Copy entries of specified directory into created
                      container root directory.
      -e             Keep container after exit if executed <command> failed.

Query options:
    -Q               Show information about containers in the <root> dir.
       <name>        Query container's options.
         --rootfs    Returns container's root FS path. Can be used to copy
                      files inside of the container.
         --ip        Returns container's IP address.
      -i             Show base images.
      -c             Show containers.
Destroy options:
    -D               Destroy specified container.
    --free           Completely remove all data in <root> directory with
                      containers and base images.

`

func main() {
	rand.Seed(time.Now().UnixNano())

	if os.Args[0] == "/.hastur.exec" && len(os.Args) >= 2 {
		err := execBootstrap()
		if err != nil {
			log.Fatal(err)
		}
	}

	args, err := docopt.Parse(usage, nil, true, "2.0", false)
	if err != nil {
		panic(err)
	}

	switch {
	case args["-S"].(bool):
		err = createAndStart(args)
	case args["-Q"].(bool):
		switch {
		default:
			err = listContainersInfo(args)
		case args["-i"].(bool):
			err = showBaseDirsInfo(args)
		case args["--rootfs"].(bool):
			err = showContainerDataRootFS(args)
		case args["--ip"].(bool):
			err = showContainerIP(args)
		}
	case args["-D"].(bool):
		err = destroyContainer(args)
	case args["--free"].(bool):
		err = destroyRoot(args)
	}

	if err != nil {
		log.Fatalf("ERROR: %s", err)
	}
}

func execBootstrap() error {
	command := []string{}

	if len(os.Args) == 2 {
		command = []string{"/bin/bash"}
	} else {
		if len(os.Args) == 3 && strings.Contains(os.Args[2], " ") {
			command = []string{"/bin/bash", "-c", os.Args[2]}
		} else {
			command = os.Args[2:]
		}
	}

	ioutil.WriteFile(os.Args[1], []byte{}, 0)
	ioutil.ReadFile(os.Args[1])

	err := os.Remove(os.Args[1])
	if err != nil {
		return fmt.Errorf(
			"can't remove control file '%s': %s", os.Args[1], err,
		)
	}

	err = syscall.Exec(command[0], command[0:], os.Environ())
	if err != nil {
		return fmt.Errorf(
			"can't execute command %q: %s", os.Args[2:], err,
		)
	}

	return nil
}

func destroyContainer(args map[string]interface{}) error {
	var (
		rootDir       = args["-r"].(string)
		force, _      = args["-f"].(bool)
		containerName = args["<name>"].(string)
	)

	containerPrivateRoot := getContainerPrivateRoot(rootDir, containerName)

	// private root can be mounted due dirty shutdown
	_ = umount(containerPrivateRoot)

	err := removeContainer(rootDir, containerName, force)
	if err != nil {
		log.Println(err)
	}

	err = umountNetorkNamespace(containerName)
	if err != nil {
		log.Println(err)
	}

	err = cleanupNetworkInterface(containerName)
	if err != nil {
		log.Println(err)
	}

	return err
}

func showBaseDirsInfo(args map[string]interface{}) error {
	var (
		rootDir = args["-r"].(string)
	)

	baseDirs, err := getBaseDirs(rootDir)
	if err != nil {
		return fmt.Errorf(
			"can't get base dirs from '%s': %s", rootDir, err,
		)
	}

	for _, baseDir := range baseDirs {
		packages, err := listExplicitlyInstalled(baseDir)
		if err != nil {
			return fmt.Errorf(
				"can't list explicitly installed packages in '%s': %s",
				baseDir,
				err,
			)
		}

		fmt.Println(baseDir)
		for _, packageName := range packages {
			fmt.Printf("\t%s\n", packageName)
		}
	}

	return nil
}

func showContainerDataRootFS(args map[string]interface{}) error {
	var (
		rootDir = args["-r"].(string)
		name    = args["<name>"].(string)
	)

	fmt.Println(getContainerDataRoot(rootDir, name))

	return nil
}

func showContainerIP(args map[string]interface{}) error {
	var (
		rootDir = args["-r"].(string)
		name    = args["<name>"].(string)
	)

	containers, err := listContainers(filepath.Join(rootDir, "containers"))
	if err != nil {
		return err
	}

	activeContainers, err := listActiveContainers(containerSuffix)
	if err != nil {
		return err
	}

	for _, containerName := range containers {
		if name != containerName {
			continue
		}

		if _, ok := activeContainers[containerName]; ok {
			address, err := getContainerIP(containerName)
			if err != nil {
				return err
			}

			fmt.Println(address)
			return nil
		} else {
			return errors.New("container is not active")
		}
	}

	return nil
}

func listContainersInfo(args map[string]interface{}) error {
	var (
		rootDir = args["-r"].(string)
	)

	containers, err := listContainers(filepath.Join(rootDir, "containers"))
	if err != nil {
		return err
	}

	activeContainers, err := listActiveContainers(containerSuffix)
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, containerName := range containers {
		address := ""
		state := "inactive"

		if _, ok := activeContainers[containerName]; ok {
			state = "running"
			address, err = getContainerIP(containerName)
			if err != nil {
				return err
			}
		}

		fmt.Fprintf(writer, "%s\t%s\t%s\n", containerName, state, address)
	}

	err = writer.Flush()
	if err != nil {
		return err
	}

	return nil
}

func createAndStart(args map[string]interface{}) error {
	var (
		bridgeInfo        = args["-b"].(string)
		rootDir           = args["-r"].(string)
		storageSpec       = args["-s"].(string)
		packagesList      = args["-p"].([]string)
		containerName, _  = args["-n"].(string)
		commandLine       = args["<command>"].([]string)
		networkAddress, _ = args["-a"].(string)
		force             = args["-f"].(bool)
		keep              = args["-k"].(bool)
		keepFailed        = args["-e"].(bool)
		copyingDir, _     = args["-x"].(string)
		hostInterface, _  = args["-t"].(string)
		quiet             = args["-q"].(bool)
	)

	err := ensureRootDir(rootDir)
	if err != nil {
		return fmt.Errorf(
			"can't create root directory at '%s': %s", rootDir, err,
		)
	}

	var bridgeDevice, bridgeAddress string
	bridgeDevice, bridgeAddress = parseBridgeInfo(bridgeInfo)
	err = ensureBridge(bridgeDevice)
	if err != nil {
		return fmt.Errorf(
			"can't create bridge interface '%s': %s", bridgeDevice, err,
		)
	}

	if bridgeAddress != "" {
		err = setupBridge(bridgeDevice, bridgeAddress)
		if err != nil {
			return fmt.Errorf(
				"can't assign address on bridge '%s': %s",
				bridgeDevice,
				err,
			)
		}
	}

	if hostInterface != "" {
		err := addInterfaceToBridge(hostInterface, bridgeDevice)
		if err != nil {
			return fmt.Errorf(
				"can't bind host's ethernet '%s' to '%s': %s",
				hostInterface,
				bridgeDevice,
				err,
			)
		}
	}

	storageEngine, err := createStorageFromSpec(rootDir, storageSpec)
	if err != nil {
		return err
	}

	err = storageEngine.Init()
	if err != nil {
		return fmt.Errorf(
			"can't init storage '%s': %s", storageSpec, err,
		)
	}

	ephemeral := false
	if containerName == "" {
		generatedName := generateContainerName()
		if !keep {
			ephemeral = true

			if !keepFailed && !quiet {
				fmt.Println(
					"Container is ephemeral and will be deleted after exit.",
				)
			}
		}

		containerName = generatedName
	}

	err = createLayout(rootDir, containerName)
	if err != nil {
		return fmt.Errorf(
			"can't create directory layout under '%s': %s", rootDir, err,
		)
	}

	allPackages := []string{}
	for _, packagesGroup := range packagesList {
		packages := strings.Split(packagesGroup, ",")
		allPackages = append(allPackages, packages...)
	}

	cacheExists, baseDir, err := createBaseDirForPackages(rootDir, allPackages)
	if err != nil {
		return fmt.Errorf(
			"can't create base dir '%s': %s", baseDir, err,
		)
	}

	if !cacheExists || force {
		fmt.Println("Installing packages")
		err = installPackages(baseDir, allPackages)
		if err != nil {
			return fmt.Errorf(
				"can't install packages into '%s': %s", rootDir, err,
			)
		}
	}

	if networkAddress == "" {
		_, baseIPNet, _ := net.ParseCIDR("10.0.0.0/8")
		networkAddress = generateRandomNetwork(baseIPNet)

		if !quiet {
			fmt.Printf("Container will use IP: %s\n", networkAddress)
		}
	}

	if copyingDir != "" {
		err = copyDir(copyingDir, baseDir)
		if err != nil {
			return fmt.Errorf(
				"can't copy %s to container root: %s", copyingDir, err,
			)
		}
	}

	err = nspawn(
		storageEngine,
		rootDir, baseDir, containerName,
		bridgeDevice, networkAddress, bridgeAddress,
		ephemeral, keepFailed, quiet,
		commandLine,
	)

	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			os.Exit(err.Sys().(syscall.WaitStatus).ExitStatus())
		}

		return fmt.Errorf("command execution failed: %s", err)
	}

	return nil
}

func destroyRoot(args map[string]interface{}) error {
	var (
		rootDir     = args["-r"].(string)
		storageSpec = args["-s"].(string)
	)

	storageEngine, err := createStorageFromSpec(rootDir, storageSpec)
	if err != nil {
		return err
	}

	err = storageEngine.Destroy()
	if err != nil {
		return fmt.Errorf(
			"can't destroy storage: %s", err,
		)
	}

	return nil
}

func generateContainerName() string {
	tuples := []string{"ir", "oh", "at", "op", "un", "ed"}
	triples := []string{"gep", "vin", "kut", "lop", "man", "zod"}
	all := append(append([]string{}, tuples...), triples...)

	getTuple := func() string {
		return tuples[rand.Intn(len(tuples))]
	}

	getTriple := func() string {
		return triples[rand.Intn(len(triples))]
	}

	getAny := func() string {
		return all[rand.Intn(len(all))]
	}

	id := []string{
		getTuple(),
		getTriple(),
		"-",
		getTuple(),
		getTriple(),
		getTuple(),
		"-",
		getAny(),
	}

	return strings.Join(id, "")
}

func parseBridgeInfo(bridgeInfo string) (dev, address string) {
	parts := strings.Split(bridgeInfo, ":")
	if len(parts) == 1 {
		return parts[0], ""
	} else {
		return parts[0], parts[1]
	}
}

func createStorageFromSpec(rootDir, storageSpec string) (storage, error) {
	var storageEngine storage
	var err error

	switch {
	case storageSpec == "autodetect":
		storageSpec = "overlayfs"
		fallthrough

	case strings.HasPrefix(storageSpec, "overlayfs"):
		storageEngine, err = NewOverlayFSStorage(rootDir, storageSpec)
	}

	if err != nil {
		return nil, fmt.Errorf(
			"can't create storage '%s': %s", storageSpec, err,
		)
	}

	return storageEngine, nil
}
