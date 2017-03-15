package topgun_test

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"

	_ "github.com/lib/pq"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = XDescribe("[#137641079] ATC Shutting down", func() {
	// pending on blockers for https://www.pivotaltracker.com/story/show/137641079
	var dbConn *sql.DB

	BeforeEach(func() {
		var err error
		dbConn, err = sql.Open("postgres", fmt.Sprintf("postgres://atc:dummy-password@%s:5432/atc?sslmode=disable", dbIP))
		Expect(err).ToNot(HaveOccurred())
	})

	Context("with two atcs available", func() {
		BeforeEach(func() {
			By("Configuring two ATCs")
			Deploy("deployments/two-atcs-one-worker.yml")
			waitForRunningWorker()
		})

		Describe("tracking builds previously tracked by shutdown ATC", func() {
			Context("with a build in-flight", func() {
				var buildID string
				var buildSession *gexec.Session

				BeforeEach(func() {
					By("stopping one of the web instances")
					stopSession := spawnBosh("stop", "web/1")
					<-stopSession.Exited
					Eventually(stopSession).Should(gexec.Exit(0))

					By("executing a task")
					buildSession = spawnFly("execute", "-c", "tasks/wait.yml")
					Eventually(buildSession).Should(gbytes.Say("executing build"))

					buildRegex := regexp.MustCompile(`executing build (\d+)`)
					matches := buildRegex.FindSubmatch(buildSession.Out.Contents())
					buildID = string(matches[1])

					Eventually(buildSession).Should(gbytes.Say("waiting for /tmp/stop-waiting"))

					By("starting the stopped web instance")
					startSession := spawnBosh("start", "web/1")
					<-startSession.Exited
					Eventually(startSession).Should(gexec.Exit(0))
					<-spawnFly("login", "-c", atcExternalURL2).Exited
				})

				Context("when the atc tracking the build shuts down", func() {
					var trackedByWeb0 int
					var waitingRegex *regexp.Regexp

					BeforeEach(func() {
						By("stopping the first web instance")
						landSession := spawnBosh("stop", "web/0")
						<-landSession.Exited
						Eventually(landSession).Should(gexec.Exit(0))

						waitingRegex = regexp.MustCompile("waiting for /tmp/stop-waiting")
						trackedByWeb0 = len(waitingRegex.FindAllString(string(buildSession.Buffer().Contents()), -1))

						restartSession := spawnBosh("start", "web/0")
						<-restartSession.Exited
						Eventually(restartSession).Should(gexec.Exit(0))
					})

					AfterEach(func() {
						restartSession := spawnBosh("start", "web/0")
						<-restartSession.Exited
						Eventually(restartSession).Should(gexec.Exit(0))
					})

					It("continues tracking the build progress", func() {
						By("waiting for another atc to attach to process")
						watchSession := spawnFly("watch", "-b", buildID)
						Eventually(func() int {
							return len(waitingRegex.FindAllString(string(watchSession.Buffer().Contents()), -1))
						}, 1*time.Minute).Should(BeNumerically(">", trackedByWeb0))

						By("hijacking the build to tell it to finish")
						hijackSession := spawnFly(
							"hijack",
							"-b", buildID,
							"-s", "one-off",
							"touch", "/tmp/stop-waiting",
						)
						<-hijackSession.Exited
						Expect(hijackSession.ExitCode()).To(Equal(0))

						By("waiting for the build to exit")
						Eventually(watchSession, 1*time.Minute).Should(gbytes.Say("done"))
						<-watchSession.Exited
						Expect(watchSession.ExitCode()).To(Equal(0))
					})
				})
			})
		})
	})
})
