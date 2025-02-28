package dockerexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	engineapi "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"

	"github.com/moby/swarmkit/v2/agent/exec"
	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/log"
)

// controller implements agent.Controller against docker's API.
//
// Most operations against docker's API are done through the container name,
// which is unique to the task.
type controller struct {
	task    *api.Task
	adapter *containerAdapter
	closed  chan struct{}
	err     error

	pulled     chan struct{} // closed after pull
	cancelPull func()        // cancels pull context if not nil
	pullErr    error         // pull error, protected by close of pulled
}

var _ exec.Controller = &controller{}

// newController returns a docker exec controller for the provided task.
func newController(client engineapi.APIClient, nodeDescription *api.NodeDescription, task *api.Task, secrets exec.SecretGetter) (exec.Controller, error) {
	adapter, err := newContainerAdapter(client, nodeDescription, task, secrets)
	if err != nil {
		return nil, err
	}

	return &controller{
		task:    task,
		adapter: adapter,
		closed:  make(chan struct{}),
	}, nil
}

// ContainerStatus returns the container-specific status for the task.
func (r *controller) ContainerStatus(ctx context.Context) (*api.ContainerStatus, error) {
	ctnr, err := r.adapter.inspect(ctx)
	if err != nil {
		if isUnknownContainer(err) {
			return nil, nil
		}

		return nil, err
	}
	return parseContainerStatus(ctnr)
}

func (r *controller) PortStatus(ctx context.Context) (*api.PortStatus, error) {
	ctnr, err := r.adapter.inspect(ctx)
	if err != nil {
		if isUnknownContainer(err) {
			return nil, nil
		}

		return nil, err
	}

	return parsePortStatus(ctnr)
}

// Update takes a recent task update and applies it to the container.
func (r *controller) Update(ctx context.Context, t *api.Task) error {
	log.G(ctx).Warnf("task updates not yet supported")
	// TODO(stevvooe): While assignment of tasks is idempotent, we do allow
	// updates of metadata, such as labelling, as well as any other properties
	// that make sense.
	return nil
}

// Prepare creates a container and ensures the image is pulled.
//
// If the container has already be created, exec.ErrTaskPrepared is returned.
func (r *controller) Prepare(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	// Make sure all the networks that the task needs are created.
	if err := r.adapter.createNetworks(ctx); err != nil {
		return err
	}

	// Make sure all the volumes that the task needs are created.
	if err := r.adapter.createVolumes(ctx); err != nil {
		return err
	}

	if r.pulled == nil {
		// Launches a re-entrant pull operation associated with controller,
		// dissociating the context from the caller's context. Allows pull
		// operation to be re-entrant on calls to prepare, resuming from the
		// same point after cancellation.
		var pctx context.Context

		r.pulled = make(chan struct{})
		pctx, r.cancelPull = context.WithCancel(context.Background()) // TODO(stevvooe): Bind a context to the entire controller.

		go func() {
			defer close(r.pulled)
			r.pullErr = r.adapter.pullImage(pctx)
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.pulled:
		if r.pullErr != nil {
			// NOTE(stevvooe): We always try to pull the image to make sure we have
			// the most up to date version. This will return an error, but we only
			// log it. If the image truly doesn't exist, the create below will
			// error out.
			//
			// This gives us some nice behavior where we use up to date versions of
			// mutable tags, but will still run if the old image is available but a
			// registry is down.
			//
			// If you don't want this behavior, lock down your image to an
			// immutable tag or digest.
			log.G(ctx).WithError(r.pullErr).Error("pulling image failed")
		}
	}

	if err := r.adapter.create(ctx); err != nil {
		if isContainerCreateNameConflict(err) {
			if _, err := r.adapter.inspect(ctx); err != nil {
				return err
			}

			// container is already created. success!
			return exec.ErrTaskPrepared
		}

		return err
	}

	return nil
}

// Start the container. An error will be returned if the container is already started.
func (r *controller) Start(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	ctnr, err := r.adapter.inspect(ctx)
	if err != nil {
		return err
	}

	// Detect whether the container has *ever* been started. If so, we don't
	// issue the start.
	//
	// TODO(stevvooe): This is very racy. While reading inspect, another could
	// start the process and we could end up starting it twice.
	if ctnr.State.Status != "created" {
		return exec.ErrTaskStarted
	}

	if err := r.adapter.start(ctx); err != nil {
		return errors.Wrap(err, "starting container failed")
	}

	// no health check
	if ctnr.Config == nil || ctnr.Config.Healthcheck == nil {
		return nil
	}

	healthCmd := ctnr.Config.Healthcheck.Test

	if len(healthCmd) == 0 {
		// this field should be filled, even if inherited from image
		// if it's empty, health check will always be at starting status
		// so treat it as no health check, and return directly
		return nil
	}

	// health check is disabled
	if healthCmd[0] == "NONE" {
		return nil
	}

	// wait for container to be healthy
	eventq, closed, err := r.adapter.events(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case event := <-eventq:
			if !r.matchevent(event) {
				continue
			}

			switch event.Action {
			case "die": // exit on terminal events
				ctnr, err := r.adapter.inspect(ctx)
				if err != nil {
					return errors.Wrap(err, "die event received")
				}

				return makeExitError(ctnr)
			case "destroy":
				// If we get here, something has gone wrong but we want to exit
				// and report anyways.
				return ErrContainerDestroyed

			case "health_status: unhealthy":
				// in this case, we stop the container and report unhealthy status
				// TODO(runshenzhu): double check if it can cause a dead lock issue here
				if err := r.Shutdown(ctx); err != nil {
					return errors.Wrap(err, "unhealthy container shutdown failed")
				}
				return ErrContainerUnhealthy

			case "health_status: healthy":
				return nil
			}
		case <-closed:
			// restart!
			eventq, closed, err = r.adapter.events(ctx)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closed:
			return r.err
		}
	}
}

// Wait on the container to exit.
func (r *controller) Wait(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	// check the initial state and report that.
	ctnr, err := r.adapter.inspect(ctx)
	if err != nil {
		return errors.Wrap(err, "inspecting container failed")
	}

	switch ctnr.State.Status {
	case "exited", "dead":
		// TODO(stevvooe): Treating container status dead as exited. There may
		// be more to do if we have dead containers. Note that this is not the
		// same as task state DEAD, which means the container is completely
		// freed on a node.

		return makeExitError(ctnr)
	}

	eventq, closed, err := r.adapter.events(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case event := <-eventq:
			if !r.matchevent(event) {
				continue
			}

			switch event.Action {
			case "die": // exit on terminal events
				ctnr, err := r.adapter.inspect(ctx)
				if err != nil {
					return errors.Wrap(err, "die event received")
				}

				return makeExitError(ctnr)
			case "destroy":
				// If we get here, something has gone wrong but we want to exit
				// and report anyways.
				return ErrContainerDestroyed

			case "health_status: unhealthy":
				// in this case, we stop the container and report unhealthy status
				// TODO(runshenzhu): double check if it can cause a dead lock issue here
				if err := r.Shutdown(ctx); err != nil {
					return errors.Wrap(err, "unhealthy container shutdown failed")
				}
				return ErrContainerUnhealthy
			}
		case <-closed:
			// restart!
			eventq, closed, err = r.adapter.events(ctx)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closed:
			return r.err
		}
	}
}

// Shutdown the container cleanly.
func (r *controller) Shutdown(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	if r.cancelPull != nil {
		r.cancelPull()
	}

	if err := r.adapter.shutdown(ctx); err != nil {
		if isUnknownContainer(err) || isStoppedContainer(err) {
			return nil
		}

		return err
	}

	return nil
}

// Terminate the container, with force.
func (r *controller) Terminate(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	if r.cancelPull != nil {
		r.cancelPull()
	}

	if err := r.adapter.terminate(ctx); err != nil {
		if isUnknownContainer(err) {
			return nil
		}

		return err
	}

	return nil
}

// Remove the container and its resources.
func (r *controller) Remove(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	if r.cancelPull != nil {
		r.cancelPull()
	}

	// It may be necessary to shut down the task before removing it.
	if err := r.Shutdown(ctx); err != nil {
		if isUnknownContainer(err) {
			return nil
		}

		// This may fail if the task was already shut down.
		log.G(ctx).WithError(err).Debug("shutdown failed on removal")
	}

	// Try removing networks referenced in this task in case this
	// task is the last one referencing it
	if err := r.adapter.removeNetworks(ctx); err != nil {
		if isUnknownContainer(err) {
			return nil
		}

		return err
	}

	if err := r.adapter.remove(ctx); err != nil {
		if isUnknownContainer(err) {
			return nil
		}

		return err
	}

	return nil
}

// waitReady waits for a container to be "ready".
// Ready means it's past the started state.
func (r *controller) waitReady(pctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	eventq, closed, err := r.adapter.events(ctx)
	if err != nil {
		return err
	}

	ctnr, err := r.adapter.inspect(ctx)
	if err != nil {
		if !isUnknownContainer(err) {
			return errors.Wrap(err, "inspect container failed")
		}
	} else {
		switch ctnr.State.Status {
		case "running", "exited", "dead":
			return nil
		}
	}

	for {
		select {
		case event := <-eventq:
			if !r.matchevent(event) {
				continue
			}

			switch event.Action {
			case "start":
				return nil
			}
		case <-closed:
			// restart!
			eventq, closed, err = r.adapter.events(ctx)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closed:
			return r.err
		}
	}
}

func (r *controller) Logs(ctx context.Context, publisher exec.LogPublisher, options api.LogSubscriptionOptions) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	if err := r.waitReady(ctx); err != nil {
		return errors.Wrap(err, "container not ready for logs")
	}

	rc, err := r.adapter.logs(ctx, options)
	if err != nil {
		return errors.Wrap(err, "failed getting container logs")
	}
	defer rc.Close()

	var (
		// use a rate limiter to keep things under control but also provides some
		// ability coalesce messages.
		limiter = rate.NewLimiter(rate.Every(time.Second), 10<<20) // 10 MB/s
		msgctx  = api.LogContext{
			NodeID:    r.task.NodeID,
			ServiceID: r.task.ServiceID,
			TaskID:    r.task.ID,
		}
	)

	brd := bufio.NewReader(rc)
	for {
		// so, message header is 8 bytes, treat as uint64, pull stream off MSB
		var header uint64
		if err := binary.Read(brd, binary.BigEndian, &header); err != nil {
			if err == io.EOF {
				return nil
			}

			return errors.Wrap(err, "failed reading log header")
		}

		stream, size := (header>>(7<<3))&0xFF, header & ^(uint64(0xFF)<<(7<<3))

		// limit here to decrease allocation back pressure.
		if err := limiter.WaitN(ctx, int(size)); err != nil {
			return errors.Wrap(err, "failed rate limiter")
		}

		buf := make([]byte, size)
		_, err := io.ReadFull(brd, buf)
		if err != nil {
			return errors.Wrap(err, "failed reading buffer")
		}

		// Timestamp is RFC3339Nano with 1 space after. Lop, parse, publish
		parts := bytes.SplitN(buf, []byte(" "), 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid timestamp in log message: %v", buf)
		}

		ts, err := time.Parse(time.RFC3339Nano, string(parts[0]))
		if err != nil {
			return errors.Wrap(err, "failed to parse timestamp")
		}

		tsp, err := gogotypes.TimestampProto(ts)
		if err != nil {
			return errors.Wrap(err, "failed to convert timestamp")
		}

		if err := publisher.Publish(ctx, api.LogMessage{
			Context:   msgctx,
			Timestamp: tsp,
			Stream:    api.LogStream(stream),

			Data: parts[1],
		}); err != nil {
			return errors.Wrap(err, "failed to publish log message")
		}
	}
}

// Close the controller and clean up any ephemeral resources.
func (r *controller) Close() error {
	select {
	case <-r.closed:
		return r.err
	default:
		if r.cancelPull != nil {
			r.cancelPull()
		}

		r.err = exec.ErrControllerClosed
		close(r.closed)
	}
	return nil
}

func (r *controller) matchevent(event events.Message) bool {
	if event.Type != events.ContainerEventType {
		return false
	}

	// TODO(stevvooe): Filter based on ID matching, in addition to name.

	// Make sure the events are for this container.
	if event.Actor.Attributes["name"] != r.adapter.container.name() {
		return false
	}

	return true
}

func (r *controller) checkClosed() error {
	select {
	case <-r.closed:
		return r.err
	default:
		return nil
	}
}

type exitError struct {
	code            int
	cause           error
	containerStatus *api.ContainerStatus
}

func (e *exitError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("task: non-zero exit (%v): %v", e.code, e.cause)
	}

	return fmt.Sprintf("task: non-zero exit (%v)", e.code)
}

func (e *exitError) ExitCode() int {
	return int(e.containerStatus.ExitCode)
}

func (e *exitError) Cause() error {
	return e.cause
}

func (e *exitError) Unwrap() error {
	return e.cause
}

func makeExitError(ctnr types.ContainerJSON) error {
	if ctnr.State.ExitCode != 0 {
		var cause error
		if ctnr.State.Error != "" {
			cause = errors.New(ctnr.State.Error)
		}

		cstatus, _ := parseContainerStatus(ctnr)
		return &exitError{
			code:            ctnr.State.ExitCode,
			cause:           cause,
			containerStatus: cstatus,
		}
	}

	return nil

}

func parseContainerStatus(ctnr types.ContainerJSON) (*api.ContainerStatus, error) {
	status := &api.ContainerStatus{
		ContainerID: ctnr.ID,
		PID:         int32(ctnr.State.Pid),
		ExitCode:    int32(ctnr.State.ExitCode),
	}

	return status, nil
}

func parsePortStatus(ctnr types.ContainerJSON) (*api.PortStatus, error) {
	status := &api.PortStatus{}

	if ctnr.NetworkSettings != nil && len(ctnr.NetworkSettings.Ports) > 0 {
		exposedPorts, err := parsePortMap(ctnr.NetworkSettings.Ports)
		if err != nil {
			return nil, err
		}
		status.Ports = exposedPorts
	}

	return status, nil
}

func parsePortMap(portMap nat.PortMap) ([]*api.PortConfig, error) {
	exposedPorts := make([]*api.PortConfig, 0, len(portMap))

	for portProtocol, mapping := range portMap {
		parts := strings.SplitN(string(portProtocol), "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port mapping: %s", portProtocol)
		}

		port, err := strconv.ParseUint(parts[0], 10, 16)
		if err != nil {
			return nil, err
		}

		var protocol api.PortConfig_Protocol
		switch strings.ToLower(parts[1]) {
		case "tcp":
			protocol = api.ProtocolTCP
		case "udp":
			protocol = api.ProtocolUDP
		case "sctp":
			protocol = api.ProtocolSCTP
		default:
			return nil, fmt.Errorf("invalid protocol: %s", parts[1])
		}

		for _, binding := range mapping {
			hostPort, err := strconv.ParseUint(binding.HostPort, 10, 16)
			if err != nil {
				return nil, err
			}

			// TODO(aluzzardi): We're losing the port `name` here since
			// there's no way to retrieve it back from the Engine.
			exposedPorts = append(exposedPorts, &api.PortConfig{
				PublishMode:   api.PublishModeHost,
				Protocol:      protocol,
				TargetPort:    uint32(port),
				PublishedPort: uint32(hostPort),
			})
		}
	}

	return exposedPorts, nil
}
