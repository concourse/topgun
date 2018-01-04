package topgun_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/credhub-cli/credhub"
	"github.com/cloudfoundry-incubator/credhub-cli/credhub/credentials/values"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	yaml "gopkg.in/yaml.v2"
)

var _ = Describe("Credhub", func() {
	pgDump := func() *gexec.Session {
		dump := exec.Command("pg_dump", "-U", "atc", "-h", dbInstance.IP, "atc")
		dump.Env = append(os.Environ(), "PGPASSWORD=dummy-password")
		dump.Stdin = bytes.NewBufferString("dummy-password\n")
		session, err := gexec.Start(dump, GinkgoWriter, GinkgoWriter)
		Expect(err).ToNot(HaveOccurred())
		<-session.Exited
		Expect(session.ExitCode()).To(Equal(0))
		return session
	}

	getPipeline := func() *gexec.Session {
		session := spawnFly("get-pipeline", "-p", "pipeline-credhub-test")
		<-session.Exited
		Expect(session.ExitCode()).To(Equal(0))
		return session
	}

	BeforeEach(func() {
		if !strings.Contains(string(bosh("releases").Out.Contents()), "credhub") {
			Skip("credhub release not uploaded")
		}

	})

	Describe("A deployment with credhub", func() {
		var credhubClient *credhub.CredHub
		BeforeEach(func() {
			Deploy(
				"deployments/concourse.yml",
				"-o", "operations/add-empty-credhub.yml",
			)

			credhubInstance := Instance("credhub")
			postgresInstance := JobInstance("postgres")

			varsDir, err := ioutil.TempDir("", "vars")
			Expect(err).ToNot(HaveOccurred())

			defer os.RemoveAll(varsDir)

			varsStore := filepath.Join(varsDir, "vars.yml")

			// generate rsa keys
			exec.Command("openssl", "genrsa", "-out", "private_key.pem", "1024")

			// generate client cert
			random := rand.Reader

			var key rsa.PrivateKey

			loadKey("private_key.pem", &key)

			now := time.Now()
			then := now.Add(60 * 60 * 24 * 365 * 1000 * 1000 * 1000) // one year
			template := x509.Certificate{
				SerialNumber: big.NewInt(1),
				Subject: pkix.Name{
					CommonName:         "creadhubCA",
					Organization:       []string{"Cloud Foundry"},
					OrganizationalUnit: []string{"app:b67446e5-b2b0-4648-a0d0-772d3d399dcb"},
				},
				//    NotBefore: time.Unix(now, 0).UTC(),
				//    NotAfter:  time.Unix(now+60*60*24*365, 0).UTC(),
				NotBefore: now,
				NotAfter:  then,

				SubjectKeyId: []byte{1, 2, 3, 4},
				KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},

				BasicConstraintsValid: true,
				IsCA: true,
				// DNSNames:              []string{"jan.newmarch.name", "localhost"},
			}
			rootCABytes, err := x509.CreateCertificate(random, &template,
				&template, &key.PublicKey, &key)
			checkError(err)

			// populate varr.yml with generated certs
			var varsContent = fmt.Sprintf(
				`
credhub_client_topgun:
	ca: |
		%s
`, string(rootCABytes))
			err = ioutil.WriteFile(varsStore, []byte(varsContent), 0644)

			Deploy(
				"deployments/concourse.yml",
				"-o", "operations/add-credhub.yml",
				"--vars-store", varsStore,
				"-v", "credhub_ip="+credhubInstance.IP,
				"-v", "postgres_ip="+postgresInstance.IP,
			)

			varsBytes, err := ioutil.ReadFile(varsStore)
			Expect(err).ToNot(HaveOccurred())

			var vars struct {
				CredHubClient struct {
					CA          string `yaml:"ca"`
					Certificate string `yaml:"certificate"`
					PrivateKey  string `yaml:"private_key"`
				} `yaml:"credhub_client_topgun"`
			}

			err = yaml.Unmarshal(varsBytes, &vars)
			Expect(err).ToNot(HaveOccurred())

			varsJson, _ := json.Marshal(vars)
			fmt.Println("vars struct content:")
			fmt.Println(string(varsJson))

			clientCert := filepath.Join(varsDir, "client.cert")
			err = ioutil.WriteFile(clientCert, []byte(vars.CredHubClient.Certificate), 0644)
			Expect(err).ToNot(HaveOccurred())

			clientKey := filepath.Join(varsDir, "client.key")
			err = ioutil.WriteFile(clientKey, []byte(vars.CredHubClient.PrivateKey), 0644)
			Expect(err).ToNot(HaveOccurred())

			credhubClient, err = credhub.New(
				"https://"+credhubInstance.IP+":8844",
				credhub.CaCerts(vars.CredHubClient.CA),
				credhub.ClientCert(clientCert, clientKey),
			)
			Expect(err).ToNot(HaveOccurred())
		})

		Context("with a pipeline build", func() {
			BeforeEach(func() {
				value, err := credhubClient.SetValue("/concourse/main/pipeline-credhub-test/resource_type_repository", values.Value("concourse/time-resource"), credhub.Overwrite)
				Expect(err).ToNot(HaveOccurred())

				fmt.Println(value)
				credhubClient.SetValue("/concourse/main/pipeline-credhub-test/time_resource_interval", values.Value("10m"), credhub.Overwrite)
				credhubClient.SetUser("/concourse/main/pipeline-credhub-test/job_secret", values.User{
					Username: "Hello",
					Password: "World",
				}, credhub.Overwrite)
				credhubClient.SetValue("/concourse/main/team_secret", values.Value("Sauce"), credhub.Overwrite)
				credhubClient.SetValue("/concourse/main/pipeline-credhub-test/image_resource_repository", values.Value("busybox"), credhub.Overwrite)

				By("setting a pipeline that contains credhub secrets")
				fly("set-pipeline", "-n", "-c", "pipelines/credential-management.yml", "-p", "pipeline-credhub-test")

				By("getting the pipeline config")
				session := getPipeline()
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("concourse/time-resource"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("10m"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("Hello/World"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("Sauce"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("busybox"))

				By("unpausing the pipeline")
				fly("unpause-pipeline", "-p", "pipeline-credhub-test")
			})

			It("parameterizes via Credhub and leaves the pipeline uninterpolated", func() {
				By("triggering job")
				watch := spawnFly("trigger-job", "-w", "-j", "pipeline-credhub-test/job-with-custom-input")
				wait(watch)
				Expect(watch).To(gbytes.Say("GET SECRET: GET-Hello/GET-World"))
				Expect(watch).To(gbytes.Say("PUT SECRET: PUT-Hello/PUT-World"))
				Expect(watch).To(gbytes.Say("GET SECRET: PUT-GET-Hello/PUT-GET-World"))
				Expect(watch).To(gbytes.Say("SECRET: Hello/World"))
				Expect(watch).To(gbytes.Say("TEAM SECRET: Sauce"))

				By("taking a dump")
				session := pgDump()
				Expect(session).ToNot(gbytes.Say("concourse/time-resource"))
				Expect(session).ToNot(gbytes.Say("10m"))
				Expect(session).To(gbytes.Say("Hello/World")) // build echoed it; nothing we can do
				Expect(session).To(gbytes.Say("Sauce"))       // build echoed it; nothing we can do
				Expect(session).ToNot(gbytes.Say("busybox"))
			})

			Context("when the job's inputs are used for a one-off build", func() {
				It("parameterizes the values using the job's pipeline scope", func() {
					By("triggering job to populate its inputs")
					watch := spawnFly("trigger-job", "-w", "-j", "pipeline-credhub-test/job-with-input")
					wait(watch)
					Expect(watch).To(gbytes.Say("GET SECRET: GET-Hello/GET-World"))
					Expect(watch).To(gbytes.Say("PUT SECRET: PUT-Hello/PUT-World"))
					Expect(watch).To(gbytes.Say("GET SECRET: PUT-GET-Hello/PUT-GET-World"))
					Expect(watch).To(gbytes.Say("SECRET: Hello/World"))
					Expect(watch).To(gbytes.Say("TEAM SECRET: Sauce"))

					By("executing a task that parameterizes image_resource")
					watch = spawnFly("execute", "-c", "tasks/credential-management-with-job-inputs.yml", "-j", "pipeline-credhub-test/job-with-input")
					wait(watch)
					Expect(watch).To(gbytes.Say("./some-resource/input"))

					By("taking a dump")
					session := pgDump()
					Expect(session).ToNot(gbytes.Say("concourse/time-resource"))
					Expect(session).ToNot(gbytes.Say("10m"))
					Expect(session).To(gbytes.Say("./some-resource/input")) // build echoed it; nothing we can do
				})
			})
		})

		Context("with a one-off build", func() {
			BeforeEach(func() {
				credhubClient.SetValue("/concourse/main/task_secret", values.Value("Hiii"), credhub.Overwrite)
				credhubClient.SetValue("/concourse/main/image_resource_repository", values.Value("busybox"), credhub.Overwrite)
			})

			It("parameterizes image_resource and params in a task config", func() {
				By("executing a task that parameterizes image_resource")
				watch := spawnFly("execute", "-c", "tasks/credential-management.yml")
				wait(watch)
				Expect(watch).To(gbytes.Say("SECRET: Hiii"))

				By("taking a dump")
				session := pgDump()
				Expect(session).ToNot(gbytes.Say("concourse/time-resource"))
				Expect(session).To(gbytes.Say("Hiii")) // build echoed it; nothing we can do
			})
		})
	})
})

func loadKey(fileName string, key interface{}) {
	inFile, err := os.Open(fileName)
	checkError(err)
	decoder := gob.NewDecoder(inFile)
	err = decoder.Decode(key)
	checkError(err)
	inFile.Close()
}

func checkError(err error) {
	if err != nil {
		fmt.Println("Fatal error ", err.Error())
		os.Exit(1)
	}
}
