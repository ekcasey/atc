package db_test

import (
	"database/sql"
	"time"

	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/db/fakes"
	"github.com/lib/pq"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("PipelineDBFactory", func() {
	var dbConn *sql.DB
	var listener *pq.Listener

	var sqlDB *db.SQLDB

	var pipelineDBFactory db.PipelineDBFactory
	var realPipelineDBFactory db.PipelineDBFactory

	var pipelinesDB *fakes.FakePipelinesDB

	BeforeEach(func() {
		postgresRunner.Truncate()

		dbConn = postgresRunner.Open()

		listener = pq.NewListener(postgresRunner.DataSourceName(), time.Second, time.Minute, nil)
		Eventually(listener.Ping, 5*time.Second).ShouldNot(HaveOccurred())
		bus := db.NewNotificationsBus(listener, dbConn)

		pipelinesDB = new(fakes.FakePipelinesDB)

		pipelineDBFactory = db.NewPipelineDBFactory(lagertest.NewTestLogger("test"), dbConn, bus, pipelinesDB)

		sqlDB = db.NewSQL(lagertest.NewTestLogger("test"), dbConn, bus)
		realPipelineDBFactory = db.NewPipelineDBFactory(lagertest.NewTestLogger("test"), dbConn, bus, sqlDB)
	})

	AfterEach(func() {
		err := dbConn.Close()
		Expect(err).NotTo(HaveOccurred())

		err = listener.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("build with team name and name", func() {
		var team, otherTeam db.SavedTeam
		var config atc.Config

		BeforeEach(func() {
			var err error
			team, err = sqlDB.SaveTeam(db.Team{Name: "some-team"})
			Expect(err).NotTo(HaveOccurred())

			otherTeam, err = sqlDB.SaveTeam(db.Team{Name: "some-other-team"})
			Expect(err).NotTo(HaveOccurred())

			config = atc.Config{
				Groups: atc.GroupConfigs{
					{
						Name:      "some-group",
						Jobs:      []string{"job-1", "job-2"},
						Resources: []string{"resource-1", "resource-2"},
					},
				},

				Resources: atc.ResourceConfigs{
					{
						Name: "some-other-resource",
						Type: "some-type",
						Source: atc.Source{
							"source-config": "some-value",
						},
					},
				},

				Jobs: atc.JobConfigs{
					{
						Name: "some-other-job",
					},
				},
			}
		})

		It("returns the specified pipeline for that team", func() {
			_, err := sqlDB.SaveConfig(team.Name, "a-pipeline-name", config, 0, db.PipelineUnpaused)
			Expect(err).NotTo(HaveOccurred())

			_, err = sqlDB.SaveConfig(otherTeam.Name, "a-pipeline-name", atc.Config{}, 0, db.PipelineUnpaused)
			Expect(err).NotTo(HaveOccurred())

			pipelineDB, err := realPipelineDBFactory.BuildWithTeamNameAndName(team.Name, "a-pipeline-name")
			Expect(err).NotTo(HaveOccurred())

			actualConfig, _, found, err := pipelineDB.GetConfig()
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(actualConfig).To(Equal(config))
		})
	})

	Describe("default pipeline", func() {
		It("is the first one returned from the DB", func() {
			savedPipelineOne := db.SavedPipeline{
				ID: 1,
				Pipeline: db.Pipeline{
					Name: "a-pipeline",
				},
			}

			savedPipelineTwo := db.SavedPipeline{
				ID: 2,
				Pipeline: db.Pipeline{
					Name: "another-pipeline",
				},
			}

			pipelinesDB.GetAllActivePipelinesReturns([]db.SavedPipeline{
				savedPipelineOne,
				savedPipelineTwo,
			}, nil)

			defaultPipelineDB, found, err := pipelineDBFactory.BuildDefault()
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())

			Expect(defaultPipelineDB.GetPipelineName()).To(Equal("a-pipeline"))
		})

		Context("when there are no pipelines", func() {
			BeforeEach(func() {
				pipelinesDB.GetAllActivePipelinesReturns([]db.SavedPipeline{}, nil)
			})

			It("returns a useful error if there are no pipelines", func() {
				_, found, err := pipelineDBFactory.BuildDefault()
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeFalse())
			})
		})
	})
})
