package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/okteto/app/cli/pkg/analytics"
	"github.com/okteto/app/cli/pkg/config"
	"github.com/okteto/app/cli/pkg/errors"
	k8Client "github.com/okteto/app/cli/pkg/k8s/client"
	"github.com/okteto/app/cli/pkg/k8s/pods"
	"github.com/okteto/app/cli/pkg/log"
	"github.com/okteto/app/cli/pkg/model"
	"github.com/okteto/app/cli/pkg/okteto"

	"github.com/okteto/app/cli/pkg/k8s/forward"
	"github.com/okteto/app/cli/pkg/syncthing"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ReconnectingMessage is the messaged show when we are trying to reconnect
const ReconnectingMessage = "Trying to reconnect to your cluster. File synchronization will automatically resume when the connection improves."

// UpContext is the common context of all operations performed during
// the up command
type UpContext struct {
	Context    context.Context
	Cancel     context.CancelFunc
	DevPath    string
	WG         *sync.WaitGroup
	Dev        *model.Dev
	Result     *okteto.Environment
	Client     *kubernetes.Clientset
	RestConfig *rest.Config
	Pod        string
	Forwarder  *forward.PortForwardManager
	Disconnect chan struct{}
	Running    chan error
	Exit       chan error
	Sy         *syncthing.Syncthing
	ErrChan    chan error
	Namespace  string
}

//Up starts a cloud dev environment
func Up() *cobra.Command {
	var devPath string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Activates your Okteto Environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Debug("starting up command")
			u := upgradeAvailable()
			if len(u) > 0 {
				log.Yellow("Okteto %s is available. To upgrade:", u)
				log.Yellow("    %s", getUpgradeCommand())
				fmt.Println()
			}

			if !syncthing.IsInstalled() {
				fmt.Println("Installing dependencies...")
				if err := downloadSyncthing(); err != nil {
					return fmt.Errorf("couldn't download syncthing, please try again")
				}
			}

			devPath = getFullPath(devPath)

			if _, err := os.Stat(devPath); os.IsNotExist(err) {
				if err := createManifest(devPath); err != nil {
					return fmt.Errorf("couldn't create your manifest: %s", err)
				}
			}

			dev, err := model.Get(devPath)
			if err != nil {
				return err
			}

			analytics.TrackUp(dev.Image, VersionString)
			return RunUp(dev, devPath)
		},
	}

	cmd.Flags().StringVarP(&devPath, "file", "f", config.ManifestFileName(), "path to the manifest file")
	return cmd
}

//RunUp starts the up sequence
func RunUp(dev *model.Dev, devPath string) error {
	up := &UpContext{
		WG:         &sync.WaitGroup{},
		Dev:        dev,
		DevPath:    filepath.Base(devPath),
		Disconnect: make(chan struct{}, 1),
		Running:    make(chan error, 1),
		Exit:       make(chan error, 1),
		ErrChan:    make(chan error, 1),
	}

	defer up.shutdown()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go up.Activate(devPath)
	select {
	case <-stop:
		log.Debugf("CTRL+C received, starting shutdown sequence")
		fmt.Println()
	case err := <-up.Exit:
		if err == nil {
			log.Debugf("finished channel received, starting shutdown sequence")
		} else {
			return err
		}
	}
	return nil
}

// Activate activates the dev environment
func (up *UpContext) Activate(devPath string) {
	up.WG.Add(1)
	defer up.WG.Done()
	var prevError error
	attach := false

	for {
		up.Context, up.Cancel = context.WithCancel(context.Background())
		progress := newProgressBar("Activating your Okteto Environment...")
		progress.start()

		err := up.devMode(attach)
		attach = true
		progress.stop()
		if err != nil {
			up.Exit <- err
			return
		}

		fmt.Println(" ✓  Okteto Environment activated")

		progress = newProgressBar("Synchronizing your files...")
		progress.start()
		err = up.startSync()
		progress.stop()
		if err != nil {
			up.Exit <- err
			return
		}

		fmt.Println(" ✓  Files synchronized")

		progress = newProgressBar("Finalizing configuration...")
		progress.start()
		err = up.forceLocalSyncState()
		progress.stop()
		if err != nil {
			up.Exit <- err
			return
		}

		switch prevError {
		case errors.ErrLostConnection:
			log.Green("Reconnected to your cluster.")
		}

		printDisplayContext("Your Okteto Environment is ready", up.Result.Name, up.Result.Endpoints)

		args := []string{"exec", "--pod", up.Pod, "--"}
		args = append(args, up.Dev.Command...)
		cmd := exec.Command(config.GetBinaryFullPath(), args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Infof("Failed to execute okteto exec: %s", err)
			up.Exit <- err
			return
		}

		log.Debugf("started new okteto exec")

		go func() {
			up.WG.Add(1)
			defer up.WG.Done()
			up.Running <- cmd.Wait()
			return
		}()

		prevError = up.WaitUntilExitOrInterrupt(cmd)
		if prevError != nil && prevError == errors.ErrLostConnection {
			log.Yellow("Connection lost to your Okteto Environment, reconnecting...")
			fmt.Println()
			up.shutdown()
			continue
		}
		if prevError != nil && prevError == errors.ErrCommandFailed && !up.Sy.IsConnected() {
			log.Yellow("Connection lost to your Okteto Environment, reconnecting...")
			fmt.Println()
			up.shutdown()
			continue
		}

		up.Exit <- nil
		return
	}
}

// WaitUntilExitOrInterrupt blocks execution until a stop signal is sent or a disconnect event or an error
func (up *UpContext) WaitUntilExitOrInterrupt(cmd *exec.Cmd) error {
	for {
		select {
		case <-up.Context.Done():
			log.Debug("context is done, sending interrupt to process")
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				log.Infof("Failed to kill process: %s", err)
			}
			return nil

		case err := <-up.Running:
			if err != nil {
				log.Infof("Command execution error: %s\n", err)
				return errors.ErrCommandFailed
			}
			return nil

		case err := <-up.ErrChan:
			log.Yellow(err.Error())
		case <-up.Disconnect:
			log.Debug("disconnected, sending interrupt to process")
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				log.Infof("Failed to kill process: %s", err)
			}
			return errors.ErrLostConnection
		}
	}
}

func (up *UpContext) devMode(isRetry bool) error {
	var err error
	up.Client, up.RestConfig, up.Namespace, err = k8Client.Get()
	if err != nil {
		return err
	}

	up.Sy, err = syncthing.New(up.Dev, up.DevPath, up.Namespace)
	if err != nil {
		return err
	}

	if up.Result, err = okteto.DevModeOn(up.Dev, up.DevPath, isRetry); err != nil {
		return err
	}

	up.Pod, err = pods.GetDevPod(up.Context, up.Dev, up.Namespace, up.Client)
	if err != nil {
		return err
	}

	return nil
}

func (up *UpContext) startSync() error {
	if err := up.Sy.Run(up.Context, up.WG); err != nil {
		return err
	}

	up.Forwarder = forward.NewPortForwardManager(up.Context, up.RestConfig, up.Client, up.ErrChan)
	if err := up.Forwarder.Add(up.Sy.RemotePort, syncthing.ClusterPort); err != nil {
		return err
	}
	if err := up.Forwarder.Add(up.Sy.RemoteGUIPort, syncthing.GUIPort); err != nil {
		return err
	}

	up.Forwarder.Start(up.Pod, up.Namespace)
	go up.Sy.Monitor(up.Context, up.WG, up.Disconnect)

	if err := up.Sy.WaitForPing(up.Context, up.WG); err != nil {
		return err
	}

	if err := up.Sy.WaitForCompletion(up.Context, up.WG, up.Dev); err != nil {
		return err
	}

	return nil
}

func (up *UpContext) forceLocalSyncState() error {
	if err := up.Sy.OverrideChanges(up.Context, up.WG, up.Dev); err != nil {
		return err
	}

	if err := up.Sy.WaitForCompletion(up.Context, up.WG, up.Dev); err != nil {
		return err
	}

	up.Sy.Type = "sendreceive"
	if err := up.Sy.UpdateConfig(); err != nil {
		return err
	}

	return up.Sy.Restart(up.Context, up.WG)
}

// Shutdown runs the cancellation sequence. It will wait for all tasks to finish for up to 500 milliseconds
func (up *UpContext) shutdown() {
	log.Debugf("cancelling context")
	if up.Cancel != nil {
		up.Cancel()
	}

	log.Debugf("waiting for tasks for be done")
	done := make(chan struct{})
	go func() {
		if up.WG != nil {
			up.WG.Wait()
		}
		close(done)
	}()

	go func() {
		if up.Forwarder != nil {
			up.Forwarder.Stop()
		}

		return
	}()

	select {
	case <-done:
		log.Debugf("completed shutdown sequence")
		return
	case <-time.After(1 * time.Second):
		log.Debugf("tasks didn't finish, terminating")
		return
	}
}

func printDisplayContext(message, name string, endpoints []string) {
	log.Success(message)
	log.Println(fmt.Sprintf("    %s     %s", log.BlueString("Name:"), name))
	if len(endpoints) > 0 {
		log.Println(fmt.Sprintf("    %s %s", log.BlueString("Endpoint:"), endpoints[0]))
	}

	fmt.Println()
}
