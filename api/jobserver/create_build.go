package jobserver

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/concourse/atc/api/present"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/dbng"
)

func (s *Server) CreateJobBuild(pipelineDB db.PipelineDB, dbPipeline dbng.Pipeline) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := s.logger.Session("create-job-build")

		jobName := r.FormValue(":job_name")

		config := pipelineDB.Config()

		job, found := config.Jobs.Lookup(jobName)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if job.DisableManualTrigger {
			w.WriteHeader(http.StatusConflict)
			return
		}

		scheduler := s.schedulerFactory.BuildScheduler(pipelineDB, dbPipeline, s.externalURL)

		resourceTypes, err := dbPipeline.ResourceTypes()
		if err != nil {
			logger.Error("failed-to-get-resource-types", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		build, _, err := scheduler.TriggerImmediately(logger, job, config.Resources, resourceTypes.Deserialize())
		if err != nil {
			logger.Error("failed-to-trigger", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "failed to trigger: %s", err)
			return
		}

		json.NewEncoder(w).Encode(present.Build(build))
	})
}
