package vtctld

import (
	"flag"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/youtube/vitess/go/vt/schemamanager/schemaswap"
	"github.com/youtube/vitess/go/vt/servenv"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/vtctl"
	"github.com/youtube/vitess/go/vt/workflow"
	"github.com/youtube/vitess/go/vt/workflow/topovalidator"
)

var (
	workflowManagerInit        = flag.Bool("workflow_manager_init", false, "Initialize the workflow manager in this vtctld instance.")
	workflowManagerUseElection = flag.Bool("workflow_manager_use_election", false, "if specified, will use a topology server-based master election to ensure only one workflow manager is active at a time.")
)

func initWorkflowManager(ts topo.Server) {
	if *workflowManagerInit {
		// Register the Topo Validators
		topovalidator.RegisterKeyspaceValidator()
		topovalidator.RegisterShardValidator()
		topovalidator.Register()

		schemaswap.RegisterWorkflowFactory()

		// Create the WorkflowManager.
		vtctl.WorkflowManager = workflow.NewManager(ts)

		// Register the long polling and websocket handlers.
		vtctl.WorkflowManager.HandleHTTPLongPolling(apiPrefix + "workflow")
		vtctl.WorkflowManager.HandleHTTPWebSocket(apiPrefix + "workflow")

		if *workflowManagerUseElection {
			runWorkflowManagerElection(ts)
		} else {
			runWorkflowManagerAlone()
		}
	}
}

func runWorkflowManagerAlone() {
	ctx, cancel := context.WithCancel(context.Background())
	go vtctl.WorkflowManager.Run(ctx)

	// Running cancel on OnTermSync will cancel the context of any
	// running workflow inside vtctld. They may still checkpoint
	// if they want to.
	servenv.OnTermSync(cancel)
}

func runWorkflowManagerElection(ts topo.Server) {
	var mp topo.MasterParticipation

	// We use servenv.ListeningURL which is only populated during Run,
	// so we have to start this with OnRun.
	servenv.OnRun(func() {
		var err error
		mp, err = ts.NewMasterParticipation("vtctld", servenv.ListeningURL.Host)
		if err != nil {
			log.Errorf("Cannot start MasterParticipation, disabling workflow manager: %v", err)
			return
		}

		// Set up a redirect host so when we are not the
		// master, we can redirect traffic properly.
		vtctl.WorkflowManager.SetRedirectFunc(func() (string, error) {
			return mp.GetCurrentMasterID()
		})

		go func() {
			for {
				ctx, err := mp.WaitForMastership()
				switch err {
				case nil:
					vtctl.WorkflowManager.Run(ctx)
				case topo.ErrInterrupted:
					return
				default:
					log.Errorf("Got error while waiting for master, will retry in 5s: %v", err)
					time.Sleep(5 * time.Second)
				}
			}
		}()
	})

	// When we get killed, clean up.
	servenv.OnTermSync(func() {
		mp.Stop()
	})
}
