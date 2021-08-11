package server

import (
	"context"
	"os"
	"time"

	"emperror.dev/errors"
	"golang.org/x/sync/semaphore"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
)

type PowerAction string

// The power actions that can be performed for a given server. This taps into the given server
// environment and performs them in a way that prevents a race condition from occurring. For
// example, sending two "start" actions back to back will not process the second action until
// the first action has been completed.
//
// This utilizes a workerpool with a limit of one worker so that all the actions execute
// in a sync manner.
const (
	PowerActionStart     = "start"
	PowerActionStop      = "stop"
	PowerActionRestart   = "restart"
	PowerActionTerminate = "kill"
)

// IsValid checks if the power action being received is valid.
func (pa PowerAction) IsValid() bool {
	return pa == PowerActionStart ||
		pa == PowerActionStop ||
		pa == PowerActionTerminate ||
		pa == PowerActionRestart
}

func (pa PowerAction) IsStart() bool {
	return pa == PowerActionStart || pa == PowerActionRestart
}

// ExecutingPowerAction checks if there is currently a power action being processed for the server.
func (s *Server) ExecutingPowerAction() bool {
	if s.powerLock == nil {
		return false
	}

	ok := s.powerLock.TryAcquire(1)
	if ok {
		s.powerLock.Release(1)
	}

	// Remember, if we acquired a lock it means nothing was running.
	return !ok
}

// HandlePowerAction is a helper function that can receive a power action and then process the
// actions that need to occur for it. This guards against someone calling Start() twice at the
// same time, or trying to restart while another restart process is currently running.
//
// However, the code design for the daemon does depend on the user correctly calling this
// function rather than making direct calls to the start/stop/restart functions on the
// environment struct.
func (s *Server) HandlePowerAction(action PowerAction, waitSeconds ...int) error {
	if s.IsInstalling() {
		return ErrServerIsInstalling
	}

	if s.IsTransferring() {
		return ErrServerIsTransferring
	}

	if s.IsRestoring() {
		return ErrServerIsRestoring
	}

	if s.powerLock == nil {
		s.powerLock = semaphore.NewWeighted(1)
	}

	// Only attempt to acquire a lock on the process if this is not a termination event. We want to
	// just allow those events to pass right through for good reason. If a server is currently trying
	// to process a power action but has gotten stuck you still should be able to pass through the
	// terminate event. The good news here is that doing that oftentimes will get the stuck process to
	// move again, and naturally continue through the process.
	if action != PowerActionTerminate {
		// Determines if we should wait for the lock or not. If a value greater than 0 is passed
		// into this function we will wait that long for a lock to be acquired.
		if len(waitSeconds) > 0 && waitSeconds[0] != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(waitSeconds[0]))
			defer cancel()

			// Attempt to acquire a lock on the power action lock for up to 30 seconds. If more
			// time than that passes an error will be propagated back up the chain and this
			// request will be aborted.
			if err := s.powerLock.Acquire(ctx, 1); err != nil {
				return errors.WithMessage(err, "could not acquire lock on power state")
			}
		} else {
			// If no wait duration was provided we will attempt to immediately acquire the lock
			// and bail out with a context deadline error if it is not acquired immediately.
			if ok := s.powerLock.TryAcquire(1); !ok {
				return errors.WithMessage(context.DeadlineExceeded, "could not acquire lock on power state")
			}
		}

		// Release the lock once the process being requested has finished executing.
		defer s.powerLock.Release(1)
	} else {
		// Still try to acquire the lock if terminating, and it is available, just so that other power
		// actions are blocked until it has completed. However, if it is unavailable we won't stop
		// the entire process.
		if ok := s.powerLock.TryAcquire(1); ok {
			// If we managed to acquire the lock be sure to released it once this process is completed.
			defer s.powerLock.Release(1)
		}
	}

	switch action {
	case PowerActionStart:
		if s.Environment.State() != environment.ProcessOfflineState {
			return ErrIsRunning
		}

		// Run the pre-boot logic for the server before processing the environment start.
		if err := s.onBeforeStart(); err != nil {
			return err
		}

		return s.Environment.Start()
	case PowerActionStop:
		// We're specifically waiting for the process to be stopped here, otherwise the lock is released
		// too soon, and you can rack up all sorts of issues.
		return s.Environment.WaitForStop(10*60, true)
	case PowerActionRestart:
		if err := s.Environment.WaitForStop(10*60, true); err != nil {
			// Even timeout errors should be bubbled back up the stack. If the process didn't stop
			// nicely, but the terminate argument was passed then the server is stopped without an
			// error being returned.
			//
			// However, if terminate is not passed you'll get a context deadline error. We could
			// probably handle that nicely here, but I'd rather just pass it back up the stack for now.
			// Either way, any type of error indicates we should not attempt to start the server back
			// up.
			return err
		}

		// Now actually try to start the process by executing the normal pre-boot logic.
		if err := s.onBeforeStart(); err != nil {
			return err
		}

		return s.Environment.Start()
	case PowerActionTerminate:
		return s.Environment.Terminate(os.Kill)
	}

	return errors.New("attempting to handle unknown power action")
}

// Execute a few functions before actually calling the environment start commands. This ensures
// that everything is ready to go for environment booting, and that the server can even be started.
func (s *Server) onBeforeStart() error {
	s.Log().Info("syncing server configuration with panel")
	if err := s.Sync(); err != nil {
		return errors.WithMessage(err, "unable to sync server data from Panel instance")
	}

	// Disallow start & restart if the server is suspended. Do this check after performing a sync
	// action with the Panel to ensure that we have the most up-to-date information for that server.
	if s.IsSuspended() {
		return ErrSuspended
	}

	// Ensure we sync the server information with the environment so that any new environment variables
	// and process resource limits are correctly applied.
	s.SyncWithEnvironment()

	// If a server has unlimited disk space, we don't care enough to block the startup to check remaining.
	// However, we should trigger a size anyway, as it'd be good to kick it off for other processes.
	if s.DiskSpace() <= 0 {
		s.Filesystem().HasSpaceAvailable(true)
	} else {
		s.PublishConsoleOutputFromDaemon("Checking server disk space usage, this could take a few seconds...")
		if err := s.Filesystem().HasSpaceErr(false); err != nil {
			return err
		}
	}

	// Update the configuration files defined for the server before beginning the boot process.
	// This process executes a bunch of parallel updates, so we just block until that process
	// is complete. Any errors as a result of this will just be bubbled out in the logger,
	// we don't need to actively do anything about it at this point, worse comes to worst the
	// server starts in a weird state and the user can manually adjust.
	s.PublishConsoleOutputFromDaemon("Updating process configuration files...")
	s.UpdateConfigurationFiles()

	if config.Get().System.CheckPermissionsOnBoot {
		s.PublishConsoleOutputFromDaemon("Ensuring file permissions are set correctly, this could take a few seconds...")
		// Ensure all the server file permissions are set correctly before booting the process.
		if err := s.Filesystem().Chown("/"); err != nil {
			return errors.WithMessage(err, "failed to chown root server directory during pre-boot process")
		}
	}

	return nil
}
