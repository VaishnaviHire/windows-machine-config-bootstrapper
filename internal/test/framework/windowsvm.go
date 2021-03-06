package framework

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/masterzen/winrm"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// remotePowerShellCmdPrefix holds the PowerShell prefix that needs to be prefixed  for every remote PowerShell
	// command executed on the remote Windows VM
	remotePowerShellCmdPrefix = "powershell.exe -NonInteractive -ExecutionPolicy Bypass "
	// sshKey is the key that will be used to access created Windows VMs
	sshKey = "libra"
	// user for the Windows node created.
	// TODO: remove this hardcoding to any user.
	user = "Administrator"
	// winRMPort is port used for WinRM communication
	winRMPort = 5986
)

// windowsVM represents a Windows VM in the test framework
type windowsVM struct {
	// cloudProvider holds the information related to cloud provider
	cloudProvider cloudprovider.Cloud
	// credentials to access the Windows VM created
	credentials *types.Credentials
	// sshClient contains the ssh client information to access the Windows VM via ssh
	sshClient *ssh.Client
	// winrmClient to access the Windows VM created
	winrmClient *winrm.Client
	// buildWMCB indicates if WSU should build WMCB and use it
	// TODO This is a WSU specific property and should be moved to wsu_test -> https://issues.redhat.com/browse/WINC-249
	buildWMCB bool
}

// WindowsVM is the interface for interacting with a Windows VM in the test framework
type WindowsVM interface {
	// CopyFile copies the given file to the remote directory in the Windows VM. The remote directory is created if it
	// does not exist
	CopyFile(string, string) error
	// RetrieveFiles retrieves the list of file from the directory in the remote Windows VM to the local host. As of
	// now, we're limiting every file in the remote directory to be written to single directory on the local host
	RetrieveFiles(string, string) error
	// Run executes the given command remotely on the Windows VM and returns the output of stdout and stderr. If the
	// bool is set, it implies that the cmd is to be execute in PowerShell.
	Run(string, bool) (string, string, error)
	// Run executes the given command remotely on the Windows VM over a ssh connection and returns the combined output
	// of stdout and stderr. If the bool is set, it implies that the cmd is to be execute in PowerShell. This function
	// should be used in scenarios where you want to execute a command that runs in the background. In these cases we
	// have observed that Run() returns before the command completes and as a result killing the process.
	RunOverSSH(string, bool) (string, error)
	// GetCredentials returns the interface for accessing the VM credentials. It is up to the caller to check if non-nil
	// Credentials are returned before usage.
	GetCredentials() *types.Credentials
	// Reinitialize re-initializes the Windows VM. Presently only the ssh client is reinitialized.
	Reinitialize() error
	// Destroy destroys the Windows VM
	Destroy() error
	// BuildWMCB returns the value of buildWMCB. It can be used by WSU to decide if it should build WMCB before using it
	BuildWMCB() bool
	// SetBuildWMCB sets the value of buildWMCB. Setting buildWMCB to true would indicate WSU will build WMCB instead of
	// downloading the latest as per the cluster version. False by default
	SetBuildWMCB(bool)
}

// newWindowsVM creates and sets up a Windows VM in the cloud and returns the WindowsVM interface that can be used to
// interact with the VM. If credentials are passed then it is assumed that VM already exists in the cloud and those
// credentials will be used to interact with the VM. If no error is returned then it is guaranteed that the VM was
// created and can be interacted with. If skipSetup is true, then configuration steps are skipped.
func newWindowsVM(imageID, instanceType string, credentials *types.Credentials, skipSetup bool) (WindowsVM, error) {
	w := &windowsVM{}
	var err error

	w.cloudProvider, err = cloudprovider.CloudProviderFactory(kubeconfig, awsCredentials, "default", artifactDir,
		imageID, instanceType, sshKey, privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("error instantiating cloud provider %v", err)
	}

	if credentials == nil {
		w.credentials, err = w.cloudProvider.CreateWindowsVM()
		if err != nil {
			return nil, fmt.Errorf("error creating Windows VM: %v", err)
		}
	} else {
		if credentials.GetIPAddress() == "" || credentials.GetPassword() == "" {
			return nil, fmt.Errorf("password or IP address not specified in credentials")
		}
		w.credentials = credentials
	}

	if err := w.setupWinRMClient(); err != nil {
		return w, fmt.Errorf("failed to setup winRM client for the Windows VM: %v", err)
	}
	// Wait for some time before starting configuring of ssh server. This is to let sshd service be available
	// in the list of services
	// TODO: Parse the output of the `Get-Service sshd, ssh-agent` on the Windows node to check if the windows nodes
	// has those services present
	if !skipSetup {
		time.Sleep(time.Minute)
		if err := w.configureOpenSSHServer(); err != nil {
			return w, fmt.Errorf("failed to configure OpenSSHServer on the Windows VM: %v", err)
		}
	}
	if err := w.getSSHClient(); err != nil {
		return w, fmt.Errorf("failed to get ssh client for the Windows VM created: %v", err)
	}

	return w, nil
}

func (w *windowsVM) CopyFile(filePath, remoteDir string) error {
	if w.sshClient == nil {
		return fmt.Errorf("CopyFile cannot be called without a SSH client")
	}

	ftp, err := sftp.NewClient(w.sshClient)
	if err != nil {
		return fmt.Errorf("sftp client initialization failed: %v", err)
	}
	defer ftp.Close()

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("error opening %s file to be transferred: %v", filePath, err)
	}
	defer f.Close()

	if err = ftp.MkdirAll(remoteDir); err != nil {
		return fmt.Errorf("error creating remote directory %s: %v", remoteDir, err)
	}

	remoteFile := remoteDir + "\\" + filepath.Base(filePath)
	dstFile, err := ftp.Create(remoteFile)
	if err != nil {
		return fmt.Errorf("error initializing %s file on Windows VMs: %v", remoteFile, err)
	}

	_, err = io.Copy(dstFile, f)
	if err != nil {
		return fmt.Errorf("error copying %s to the Windows VM: %v", filePath, err)
	}

	// Forcefully close it so that we can execute the binary later
	dstFile.Close()
	return nil
}

// RetrieveFiles retrieves list of files from remote directory to the local directory.
// The implementation can be changed if the use-case arises. As of now, we're doing a best effort
// to collect every log possible. If a retrieval of file fails, we would proceed with retrieval
// of other log files.
func (w *windowsVM) RetrieveFiles(remoteDir, localDir string) error {
	if w.sshClient == nil {
		return fmt.Errorf("RetrieveFile cannot be called without a ssh client")
	}

	// Create local dir
	err := os.MkdirAll(localDir, os.ModePerm)
	if err != nil {
		log.Printf("could not create %s: %s", localDir, err)
	}

	sftp, err := sftp.NewClient(w.sshClient)
	if err != nil {
		return fmt.Errorf("sftp initialization failed: %v", err)
	}
	defer sftp.Close()

	// Get the list of all files in the directory
	remoteFiles, err := sftp.ReadDir(remoteDir)
	if err != nil {
		return fmt.Errorf("error opening remote file: %v", err)
	}

	for _, remoteFile := range remoteFiles {
		// Assumption: We ignore the directories here the reason being RetrieveFiles should just retrieve files
		// in a directory, if this is directory, we should have called RetrieveFiles on this directory
		if remoteFile.IsDir() {
			continue
		}
		fileName := remoteFile.Name()
		dstFile, err := os.Create(filepath.Join(localDir, fileName))
		if err != nil {
			log.Printf("error creating file locally: %v", err)
			continue
		}
		// TODO: Check if there is some performance implication of multiple Open calls.
		srcFile, err := sftp.Open(remoteDir + "\\" + fileName)

		if err != nil {
			log.Printf("error while opening remote directory on the Windows VM: %v", err)
			continue
		}
		_, err = io.Copy(dstFile, srcFile)
		if err != nil {
			log.Printf("error retrieving file %v from Windows VM: %v", fileName, err)
			continue
		}
		// flush memory
		if err = dstFile.Sync(); err != nil {
			log.Printf("error flusing memory: %v", err)
			continue
		}
		if err := srcFile.Close(); err != nil {
			log.Printf("error closing file on the remote host %s", fileName)
			continue
		}
		if err := dstFile.Close(); err != nil {
			log.Printf("error closing file %s locally", fileName)
			continue
		}
	}
	return nil
}

func (w *windowsVM) Run(cmd string, psCmd bool) (string, string, error) {
	if w.winrmClient == nil {
		return "", "", fmt.Errorf("Run cannot be called without a WinRM client")
	}

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	if psCmd {
		cmd = remotePowerShellCmdPrefix + cmd
	}
	// Remotely execute the test binary.
	exitCode, err := w.winrmClient.Run(cmd, stdout, stderr)
	if err != nil {
		return "", "", fmt.Errorf("error while executing %s remotely: %v", cmd, err)
	}

	if exitCode != 0 {
		return stdout.String(), stderr.String(), fmt.Errorf("%s returned %d exit code", cmd, exitCode)
	}

	return stdout.String(), stderr.String(), nil
}

func (w *windowsVM) RunOverSSH(cmd string, psCmd bool) (string, error) {
	if w.sshClient == nil {
		return "", fmt.Errorf("RunOverSSH cannot be called without a ssh client")
	}

	session, err := w.sshClient.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	if psCmd {
		cmd = remotePowerShellCmdPrefix + cmd
	}

	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (w *windowsVM) GetCredentials() *types.Credentials {
	return w.credentials
}

func (w *windowsVM) Reinitialize() error {
	if err := w.getSSHClient(); err != nil {
		return fmt.Errorf("failed to reinitialize ssh client: %v", err)
	}
	return nil
}

func (w *windowsVM) Destroy() error {
	// There is no VM to destroy
	if w.cloudProvider == nil || w.credentials == nil {
		return nil
	}
	return w.cloudProvider.DestroyWindowsVMs()
}

// setupWinRMClient sets up the winrm client to be used while accessing Windows node
func (w *windowsVM) setupWinRMClient() error {
	host := w.credentials.GetIPAddress()
	password := w.credentials.GetPassword()

	// Connect to the bootstrapped host. Timeout is high as the Windows Server image is slow to download
	endpoint := winrm.NewEndpoint(host, winRMPort, true, true,
		nil, nil, nil, time.Minute*10)
	winrmClient, err := winrm.NewClient(endpoint, user, password)
	if err != nil {
		return fmt.Errorf("failed to set up winrm client with error: %v", err)
	}
	w.winrmClient = winrmClient
	return nil
}

// configureOpenSSHServer configures the OpenSSH server using WinRM client installed on the Windows VM.
// The OpenSSH server is installed as part of WNI tool's CreateVM method.
func (w *windowsVM) configureOpenSSHServer() error {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	// This dependency is needed for the subsequent module installation we're doing. This version of NuGet
	// needed for OpenSSH server 0.0.1
	installDependentPackages := "Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force"
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+installDependentPackages, stdout, stderr); err != nil {
		return fmt.Errorf("failed to install dependent packages for OpenSSH server with error %v", err)
	}
	// Configure OpenSSH for all users.
	// TODO: Limit this to Administrator.
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+"Install-Module -Force OpenSSHUtils -Scope AllUsers",
		stdout, stderr); err != nil {
		return fmt.Errorf("failed to configure OpenSSHUtils for all users: %v", err)
	}
	// Setup ssh-agent Windows Service.
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+"Set-Service -Name ssh-agent -StartupType ‘Automatic’",
		stdout, stderr); err != nil {
		return fmt.Errorf("failed to set up ssh-agent Windows Service: %v", err)
	}
	// Setup sshd Windows service
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+"Set-Service -Name sshd -StartupType ‘Automatic’",
		stdout, stderr); err != nil {
		return fmt.Errorf("failed to set up sshd Windows Service: %v", err)
	}
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+"Start-Service ssh-agent",
		stdout, stderr); err != nil {
		return fmt.Errorf("start ssh-agent failed: %v", err)
	}
	if _, err := w.winrmClient.Run(remotePowerShellCmdPrefix+"Start-Service sshd", stdout, stderr); err != nil {
		return fmt.Errorf("failed to start sshd: %v", err)
	}
	return nil
}

// getSSHClient gets the ssh client associated with Windows VM created
func (w *windowsVM) getSSHClient() error {
	if w.sshClient != nil {
		// Close the existing client to be on the safe side
		if err := w.sshClient.Close(); err != nil {
			log.Printf("error closing ssh client connection: %v", err)
		}
	}

	config := &ssh.ClientConfig{
		User:            "Administrator",
		Auth:            []ssh.AuthMethod{ssh.Password(w.credentials.GetPassword())},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	sshClient, err := ssh.Dial("tcp", w.credentials.GetIPAddress()+":22", config)
	if err != nil {
		return fmt.Errorf("failed to dial to ssh server: %s", err)
	}
	w.sshClient = sshClient
	return nil
}

func (w *windowsVM) BuildWMCB() bool {
	return w.buildWMCB
}

func (w *windowsVM) SetBuildWMCB(buildWMCB bool) {
	w.buildWMCB = buildWMCB
}
