package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type signalTestAdapter struct{ marker string }

func (signalTestAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "signaltest", Label: "Signal fixture", ProcessNames: []string{"signaltest"}, OwnerRole: "client", Executable: "signaltest"}
}

func (signalTestAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{IdleWake: true, Delivery: "signal_fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryBetweenTools}
}

func (a signalTestAdapter) Deliver(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ready := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(server, make([]byte, 1))
		if err == nil {
			err = os.WriteFile(a.marker, []byte("one byte consumed; remaining write blocked"), 0600)
		}
		ready <- err
	}()
	// Match a real transport timeout as well as request cancellation. The SDK
	// may wait for an active handler when the server context stops.
	if err := client.SetWriteDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		return model.NewReceipt(model.StatusNotSent).WithCause(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = client.SetWriteDeadline(time.Now()) })
	defer stop()
	_, writeErr := client.Write([]byte("fixture frame: " + msg.Body))
	if readErr := <-ready; readErr != nil {
		writeErr = readErr
	}
	receipt := model.NewReceipt(model.StatusUnknownWrite)
	receipt.Error = "fixture write interrupted"
	if ctx.Err() != nil {
		writeErr = ctx.Err()
	}
	return receipt.WithCause(writeErr)
}

func signalTestRegistry(marker string) *agent.Registry {
	registry, err := agent.NewRegistry(signalTestAdapter{marker: marker})
	if err != nil {
		panic(err)
	}
	return registry
}

func TestMCPSignalProcessHelper(*testing.T) {
	if os.Getenv("ZALKI_SIGNAL_PROCESS_HELPER") != "1" {
		return
	}
	state := os.Getenv("ZALKI_SIGNAL_STATE")
	opts := &commandOptions{home: filepath.Dir(state), state: state, logLevel: "debug", logFile: "-", registry: signalTestRegistry(os.Getenv("ZALKI_SIGNAL_MARKER"))}
	root := &cobra.Command{Use: "signal-helper", SilenceErrors: true, SilenceUsage: true}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return opts.initLogger(cmd) }
	root.AddCommand(newMCPCommand(opts))
	observeCommands(root, opts)
	root.SetArgs([]string{"mcp"})
	root.SetIn(os.Stdin)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ctx = context.WithValue(ctx, signalContextKey{}, ctx)
	err := root.ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zalki:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
