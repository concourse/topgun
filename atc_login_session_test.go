package topgun_test

import (
	"encoding/json"
	"net/http"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/concourse/atc"
	_ "github.com/lib/pq"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Multiple ATCs Login Session Test", func() {
	Context("with two atcs available", func() {
		var atcs []boshInstance
		var atc0URL string
		var atc1URL string
		var client *http.Client
		var manifestFile string

		JustBeforeEach(func() {
			By("Configuring two ATCs")
			Deploy(manifestFile)
			waitForRunningWorker()

			atcs = JobInstances("atc")
			atc0URL = "http://" + atcs[0].IP + ":8080"
			atc1URL = "http://" + atcs[1].IP + ":8080"
		})

		AfterEach(func() {
			restartSession := spawnBosh("start", atcs[0].Name)
			<-restartSession.Exited
			Eventually(restartSession).Should(gexec.Exit(0))
		})

		Context("Using database storage for dex", func() {
			BeforeEach(func() {
				manifestFile = "deployments/concourse-two-atcs-slow-tracking.yml"
			})

			It("stores the dex tables under the dex schema", func() {
				var schemaName string

				err := psql.Select("table_schema").From("information_schema.tables").Where(sq.Eq{"table_name": "auth_request"}).RunWith(dbConn).QueryRow().Scan(&schemaName)
				Expect(err).ToNot(HaveOccurred())
				Expect(schemaName).To(Equal("dex"))
			})

			It("uses the same client for multiple ATCs", func() {
				var numClient int
				err := psql.Select("COUNT(*)").From("dex.client").RunWith(dbConn).QueryRow().Scan(&numClient)
				Expect(err).ToNot(HaveOccurred())
				Expect(numClient).To(Equal(1))
			})
		})

		Context("make api request to a different atc by a token from a stopped atc", func() {
			BeforeEach(func() {
				manifestFile = "deployments/concourse-two-atcs-slow-tracking.yml"
			})

			It("request successfully", func() {
				var (
					err       error
					request   *http.Request
					response  *http.Response
					reqHeader http.Header
				)

				By("stopping the first atc")
				stopSession := spawnBosh("stop", atcs[0].Name)
				Eventually(stopSession).Should(gexec.Exit(0))

				token, err := fetchToken(atc0URL, "test", "test")
				Expect(err).ToNot(HaveOccurred())
				reqHeader = http.Header{}
				reqHeader.Set("Authorization", "Bearer "+token.AccessToken)

				By("make request with the token to second atc")
				request, err = http.NewRequest("GET", atc1URL+"/api/v1/workers", nil)
				request.Header = reqHeader
				Expect(err).NotTo(HaveOccurred())

				response, err = client.Do(request)
				Expect(err).NotTo(HaveOccurred())

				var workers []atc.Worker
				err = json.NewDecoder(response.Body).Decode(&workers)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when two atcs have the same external url (dex redirect uri is the same)", func() {
			BeforeEach(func() {
				manifestFile = "deployments/concourse-two-atcs-with-same-redirect-uri.yml"
			})

			It("is able to login to both atcs", func() {
				Eventually(func() *gexec.Session {
					return flyLogin("-c", atc0URL).Wait()
				}, 2*time.Minute).Should(gexec.Exit(0))

				Eventually(func() *gexec.Session {
					return flyLogin("-c", atc1URL).Wait()
				}, 2*time.Minute).Should(gexec.Exit(0))
			})
		})
	})
})
