package topgun_test

import (
	"bytes"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/aws-sdk-go/service/sts"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

/*
	These tests assume that the parameters are already present in AWS. So require only GetParameter creadentials.
	To initialize test paramters in AWS, execute following commands:

	aws ssm put-parameter --type String --name "/concourse-topgun/main/pipeline-ssm-test/resource_type_repository" --value "concourse/time-resource"
	aws ssm put-parameter --type String --name "/concourse-topgun/main/pipeline-ssm-test/time_resource_interval" --value "10m"
	aws ssm put-parameter --type String --name "/concourse-topgun/main/pipeline-ssm-test/image_resource_repository" --value "busybox"
	aws ssm put-parameter --type SecureString --name "/concourse-topgun/main/pipeline-ssm-test/job_secret/username" --value "Hello"
	aws ssm put-parameter --type SecureString --name "/concourse-topgun/main/pipeline-ssm-test/job_secret/password" --value "World"
	aws ssm put-parameter --type SecureString --name "/concourse-topgun/main/team_secret" --value "Sauce"
	aws ssm put-parameter --type SecureString --name "/concourse-topgun/main/task_secret" --value "Hiii"
	aws ssm put-parameter --type SecureString --name "/concourse-topgun/main/image_resource_repository" --value "busybox"
*/
var _ = Describe("AWS SSM", func() {
	const team = "main"
	const pipeline = "pipeline-ssm-test"

	getPipeline := func() *gexec.Session {
		session := spawnFly("get-pipeline", "-p", pipeline)
		<-session.Exited
		Expect(session.ExitCode()).To(Equal(0))
		return session
	}

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

	var awsRegion string
	var awsAccessKeyID, awsSecretAccessKey, awsSessionToken string

	BeforeEach(func() {
		awsRegion = os.Getenv("AWS_SSM_REGION")
		awsAccessKeyID = os.Getenv("AWS_SSM_ACCESS_KEY_ID")
		awsSecretAccessKey = os.Getenv("AWS_SSM_SECRET_ACCESS_KEY")
		awsSessionToken = os.Getenv("AWS_SSM_SESSION_TOKEN")

		if awsRegion == "" || awsAccessKeyID == "" || awsSecretAccessKey == "" {
			Skip("must set $AWS_SSM_REGION, $AWS_SSM_ACCESS_KEY_ID, $AWS_SSM_SECRET_ACCESS_KEY, and optionally $AWS_SSM_SESSION_TOKEN to run AWS SSM tests")
		}

		awsSession, err := session.NewSession(&aws.Config{
			Region: aws.String(awsRegion),
			Credentials: credentials.NewStaticCredentials(
				awsAccessKeyID,
				awsSecretAccessKey,
				awsSessionToken,
			),
		})
		Expect(err).ToNot(HaveOccurred())

		if awsSessionToken == "" {
			stsApi := sts.New(awsSession)

			token, err := stsApi.GetSessionToken(nil)
			Expect(err).ToNot(HaveOccurred())

			awsAccessKeyID = *token.Credentials.AccessKeyId
			awsSecretAccessKey = *token.Credentials.SecretAccessKey
			awsSessionToken = *token.Credentials.SessionToken
		}

		// Verify that all secret values are present in SSM parameter store
		var secrets = map[string]string{
			"/concourse-topgun/main/pipeline-ssm-test/resource_type_repository":  "concourse/time-resource",
			"/concourse-topgun/main/pipeline-ssm-test/time_resource_interval":    "10m",
			"/concourse-topgun/main/pipeline-ssm-test/job_secret/username":       "Hello",
			"/concourse-topgun/main/pipeline-ssm-test/job_secret/password":       "World",
			"/concourse-topgun/main/pipeline-ssm-test/image_resource_repository": "busybox",
			"/concourse-topgun/main/team_secret":                                 "Sauce",
			"/concourse-topgun/main/task_secret":                                 "Hiii",
			"/concourse-topgun/main/image_resource_repository":                   "busybox",
		}
		names := make([]*string, 0, len(secrets))
		for n := range secrets {
			names = append(names, aws.String(n))
		}

		ssmApi := ssm.New(awsSession)

		result, err := ssmApi.GetParameters(&ssm.GetParametersInput{Names: names, WithDecryption: aws.Bool(true)})
		Expect(err).To(BeNil())
		Expect(result.InvalidParameters).To(BeEmpty())
		for _, p := range result.Parameters {
			Expect(p).ToNot(BeNil())
			Expect(p.Name).ToNot(BeNil())
			Expect(p.Value).ToNot(BeNil())
			Expect(secrets).To(HaveKeyWithValue(*p.Name, *p.Value))
		}
	})

	Describe("A deployment with SSM", func() {
		BeforeEach(func() {
			Deploy(
				"deployments/concourse.yml",
				"-o", "operations/configure-ssm.yml",
				"-v", "aws_region="+awsRegion,
				"-v", "aws_access_key="+awsAccessKeyID,
				"-v", "aws_secret_key="+awsSecretAccessKey,
				"-v", "aws_session_token="+awsSessionToken,
			)
		})
		Context("with a pipeline build", func() {
			BeforeEach(func() {
				By("setting a pipeline that contains ssm secrets")
				fly("set-pipeline", "-n", "-c", "pipelines/credential-management.yml", "-p", pipeline)

				By("getting the pipeline config")
				session := getPipeline()
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("concourse/time-resource"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("10m"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("Hello/World"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("Sauce"))
				Expect(string(session.Out.Contents())).ToNot(ContainSubstring("busybox"))

				By("unpausing the pipeline")
				fly("unpause-pipeline", "-p", pipeline)
			})
			It("parameterizes via SSM and leaves the pipeline uninterpolated", func() {
				By("triggering job")
				watch := spawnFly("trigger-job", "-w", "-j", pipeline+"/job-with-custom-input")
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
					watch := spawnFly("trigger-job", "-w", "-j", "pipeline-ssm-test/job-with-input")
					wait(watch)
					Expect(watch).To(gbytes.Say("GET SECRET: GET-Hello/GET-World"))
					Expect(watch).To(gbytes.Say("PUT SECRET: PUT-Hello/PUT-World"))
					Expect(watch).To(gbytes.Say("GET SECRET: PUT-GET-Hello/PUT-GET-World"))
					Expect(watch).To(gbytes.Say("SECRET: Hello/World"))
					Expect(watch).To(gbytes.Say("TEAM SECRET: Sauce"))

					By("executing a task that parameterizes image_resource")
					watch = spawnFly("execute", "-c", "tasks/credential-management-with-job-inputs.yml", "-j", "pipeline-ssm-test/job-with-input")
					wait(watch)
					Expect(watch).To(gbytes.Say("./some-resource/input"))

					By("taking a dump")
					session := pgDump()
					Expect(session).ToNot(gbytes.Say("concourse/time-resource"))
					Expect(session).ToNot(gbytes.Say("10m"))
					Expect(session).To(gbytes.Say("./some-resource/input")) // build echoed it; nothing we can do
				})
			})
			Context("with a one-off build", func() {
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
})
