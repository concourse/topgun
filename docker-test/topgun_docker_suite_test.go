package topgun_test

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	// "path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	gclient "code.cloudfoundry.org/garden/client"
	gconn "code.cloudfoundry.org/garden/client/connection"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	sq "github.com/Masterminds/squirrel"
	"github.com/concourse/concourse/go-concourse/concourse"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"golang.org/x/oauth2"
)

var (
	deploymentName, flyTarget string
	instances                 map[string][]boshInstance
	jobInstances              map[string][]boshInstance

	dbInstance *boshInstance
	dbConn     *sql.DB

	atcInstance    *boshInstance
	atcExternalURL string
	atcUsername    string
	atcPassword    string

	concourseReleaseVersion, gardenRuncReleaseVersion, postgresReleaseVersion string
	gitServerReleaseVersion, vaultReleaseVersion, credhubReleaseVersion       string
	stemcellVersion                                                           string

	pipelineName string

	flyBin string

	logger *lagertest.TestLogger

	tmp string

	deploymentLogs *gexec.Session
)

var psql = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

func TestTOPGUN(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TOPGUN Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	flyBinPath, err := gexec.Build("github.com/concourse/concourse/fly")
	Expect(err).ToNot(HaveOccurred())

	return []byte(flyBinPath)
}, func(data []byte) {
	flyBin = string(data)
})

var _ = SynchronizedAfterSuite(func() {
}, func() {
	gexec.CleanupBuildArtifacts()
})

var _ = BeforeEach(func() {
	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
	SetDefaultConsistentlyDuration(time.Minute)
	SetDefaultConsistentlyPollingInterval(time.Second)

	logger = lagertest.NewTestLogger("test")

	var err error

	Expect(err).NotTo(HaveOccurred())

	// TODO: use these variable with docker.
	concourseReleaseVersion = os.Getenv("CONCOURSE_RELEASE_VERSION")
	if concourseReleaseVersion == "" {
		concourseReleaseVersion = "latest"
	}

	gardenRuncReleaseVersion = os.Getenv("GARDEN_RUNC_RELEASE_VERSION")
	if gardenRuncReleaseVersion == "" {
		gardenRuncReleaseVersion = "latest"
	}

	postgresReleaseVersion = os.Getenv("POSTGRES_RELEASE_VERSION")
	if postgresReleaseVersion == "" {
		postgresReleaseVersion = "latest"
	}

	gitServerReleaseVersion = os.Getenv("GIT_SERVER_RELEASE_VERSION")
	if gitServerReleaseVersion == "" {
		gitServerReleaseVersion = "latest"
	}

	vaultReleaseVersion = os.Getenv("VAULT_RELEASE_VERSION")
	if vaultReleaseVersion == "" {
		vaultReleaseVersion = "latest"
	}

	credhubReleaseVersion = os.Getenv("CREDHUB_RELEASE_VERSION")
	if credhubReleaseVersion == "" {
		credhubReleaseVersion = "latest"
	}

	deploymentNumber := GinkgoParallelNode()

	deploymentName = fmt.Sprintf("concourse-topgun-%d", deploymentNumber)
	flyTarget = deploymentName

	tmp, err = ioutil.TempDir("", "topgun-tmp")
	Expect(err).ToNot(HaveOccurred())

	dockerCompose("down")

	instances = map[string][]boshInstance{}
	jobInstances = map[string][]boshInstance{}

	dbInstance = nil
	dbConn = nil
	atcInstance = nil
	atcExternalURL = ""
	atcUsername = "test"
	atcPassword = "test"
})

var _ = AfterEach(func() {
	if deploymentLogs != nil {
		deploymentLogs.Signal(os.Interrupt)
		<-deploymentLogs.Exited
		deploymentLogs =nil
	}

	deleteAllContainers()

	dockerCompose("down")

	Expect(os.RemoveAll(tmp)).To(Succeed())
})

func requestCredsInfo(atcUrl string) ([]byte, error) {
	request, err := http.NewRequest("GET", atcUrl+"/api/v1/info/creds", nil)
	Expect(err).ToNot(HaveOccurred())

	reqHeader := http.Header{}
	token, err := fetchToken(atcUrl, "some-user", "password")
	Expect(err).ToNot(HaveOccurred())

	reqHeader.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header = reqHeader

	client := &http.Client{}
	resp, err := client.Do(request)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(200))

	body, err := ioutil.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())

	return body, err
}

func StartDeploy(manifest string, args ...string) *gexec.Session {
	//TODO Do we need to specify the Docker Compose file here ?
	return spawnDockerCompose(
		append([]string{
		"-f", manifest, 
		"up",
		"-d",
		},
		args...
	)...)
}

func Deploy(manifest string, args ...string) {
	//TODO Implement Docker compose down & cleanup logic here
	if deploymentLogs != nil {
		deploymentLogs.Signal(os.Interrupt)
		<-deploymentLogs.Exited
		deploymentLogs =nil
	}

	if dbConn != nil {
		Expect(dbConn.Close()).To(Succeed())
	}

	wait(StartDeploy(manifest, args...))

	//TODO Refactor this for Docker
	instances, jobInstances = loadJobInstances()

	deploymentLogs = spawnDockerCompose("logs", "-f")

	time.Sleep(20 * time.Second)
	// for _, is := range instances {
	// 	for _, i := range is {
	// 		By("waiting for logs from " + i.Name)
	// 		Eventually(deploymentLogs.Out.Contents).Should(ContainSubstring(i.Name))
	// 	}
	// }

	// atcInstance = JobInstance("atc")
	// if atcInstance != nil {
		// give some time for atc to bootstrap (Run migrations, etc)
	
	atcExternalURL = fmt.Sprintf("http://%s", JobAddressFromHost("web", 0))
	Eventually(func() *gexec.Session {
		return flyLogin("-c", atcExternalURL).Wait()
	}, 30*time.Second).Should(gexec.Exit(0))
	// }

	
	// dbInstance = JobInstance("postgres")

	// if dbInstance != nil {
	var err error
	dbConn, err = sql.Open("postgres", fmt.Sprintf("postgres://atc:dummy-password@%s/atc?sslmode=disable", JobAddressFromHost("db", 0)))
	Expect(err).ToNot(HaveOccurred())
	// }
}

func Instance(name string) *boshInstance {
	is := instances[name]
	if len(is) == 0 {
		return nil
	}

	return &is[0]
}

func Instances(name string) []boshInstance {
	return instances[name]
}

func JobInstance(job string) *boshInstance {
	is := jobInstances[job]
	if len(is) == 0 {
		return nil
	}

	return &is[0]
}

func JobInstances(job string) []boshInstance {
	return jobInstances[job]
}

type boshInstance struct {
	Name string
	IP   string
}
func JobContainerId(job string, index int) string {
	session := docker("ps", "-q", "-f", "name=" + deploymentName + "_" + job + "_" + strconv.Itoa(index + 1))
	return strings.TrimSpace(string(session.Out.Contents()))
}

func JobAddressFromHost(job string, index int) string{
	port := "8080"
	// Index starts at 1
	indexStr := fmt.Sprintf("--index=%d", index + 1)
	if job == "garden" {
		port = "7777"
	} else if job == "baggageclaim" {
		port = "7788"
	} else if job == "db" {
		port = "5432"
	}

	session := dockerCompose("port", indexStr, job, port)
	return  strings.TrimSpace(string(session.Out.Contents()))
}
var instanceRow = regexp.MustCompile(`^([^/]+)/([^\s]+)\s+-\s+(\w+)\s+z1\s+([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)\s*$`)
var jobRow = regexp.MustCompile(`^([^\s]+)\s+(\w+)\s+(\w+)\s+-\s+-\s*$`)

func loadJobInstances() (map[string][]boshInstance, map[string][]boshInstance) {
	_ = dockerCompose("ps")

	// output := string(session.Out.Contents())

	instances := map[string][]boshInstance{}
	jobInstances := map[string][]boshInstance{}

	//TODO Implement docker ps output parsing to implement instances map
	// lines := strings.Split(output, "\n")
	// var instance boshInstance
	// for _, line := range lines {
	// 	instanceMatch := instanceRow.FindStringSubmatch(line)
	// 	if len(instanceMatch) > 0 {
	// 		group := instanceMatch[1]
	// 		id := instanceMatch[2]

	// 		instance = boshInstance{
	// 			Name: group + "/" + id,
	// 			IP:   instanceMatch[4],
	// 		}

	// 		instances[group] = append(instances[group], instance)

	// 		continue
	// 	}

	// 	jobMatch := jobRow.FindStringSubmatch(line)
	// 	if len(jobMatch) > 0 {
	// 		jobName := jobMatch[2]
	// 		jobInstances[jobName] = append(jobInstances[jobName], instance)
	// 	}
	// }

	return instances, jobInstances
}

func dockerCompose(argv ...string) *gexec.Session {
	session := spawnDockerCompose(argv...)
	wait(session)
	return session
}

func spawnDockerCompose(argv ...string) *gexec.Session {
	return spawn("docker-compose", append([]string{"-p", deploymentName}, argv...)...)
}
func docker(argv ...string) *gexec.Session {
	session := spawnDocker(argv...)
	wait(session)
	return session
}
func spawnDocker(argv ...string) *gexec.Session {
	return spawn("docker", argv...)
}

func fly(argv ...string) {
	wait(spawnFly(argv...))
}

func concourseClient() concourse.Client {
	token, err := fetchToken(atcExternalURL, atcUsername, atcPassword)
	Expect(err).NotTo(HaveOccurred())

	httpClient := &http.Client{
		Transport: &oauth2.Transport{
			Source: oauth2.StaticTokenSource(token),
			Base: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}

	return concourse.NewClient(atcExternalURL, httpClient, false)
}

func fetchToken(atcURL string, username, password string) (*oauth2.Token, error) {
	oauth2Config := oauth2.Config{
		ClientID:     "fly",
		ClientSecret: "Zmx5",
		Endpoint:     oauth2.Endpoint{TokenURL: atcURL + "/sky/token"},
		Scopes:       []string{"openid", "profile", "email", "federated:id"},
	}

	return oauth2Config.PasswordCredentialsToken(context.Background(), username, password)
}

func deleteAllContainers() {
	client := concourseClient()
	workers, err := client.ListWorkers()
	Expect(err).NotTo(HaveOccurred())

	mainTeam := client.Team("main")
	containers, err := mainTeam.ListContainers(map[string]string{})
	Expect(err).NotTo(HaveOccurred())

	for _, worker := range workers {
		connection := gconn.New("tcp", worker.GardenAddr)
		gardenClient := gclient.New(connection)
		for _, container := range containers {
			if container.WorkerName == worker.Name {
				err = gardenClient.Destroy(container.ID)
				if err != nil {
					logger.Error("failed-to-delete-container", err, lager.Data{"handle": container.ID})
				}
			}
		}
	}
}

func flyHijackTask(argv ...string) *gexec.Session {
	cmd := exec.Command(flyBin, append([]string{"-t", flyTarget, "hijack"}, argv...)...)
	hijackIn, err := cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred())

	hijackS, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() bool {
		taskMatcher := gbytes.Say("type: task")
		matched, err := taskMatcher.Match(hijackS)
		Expect(err).ToNot(HaveOccurred())

		if matched {
			re, err := regexp.Compile("([0-9]): .+ type: task")
			Expect(err).NotTo(HaveOccurred())

			taskNumber := re.FindStringSubmatch(string(hijackS.Out.Contents()))[1]
			fmt.Fprintln(hijackIn, taskNumber)

			return true
		}

		return hijackS.ExitCode() == 0
	}).Should(BeTrue())

	return hijackS
}

func flyLogin(args ...string) *gexec.Session {
	return spawnFly(append([]string{"login", "-u", atcUsername, "-p", atcPassword}, args...)...)
}

func spawnFly(argv ...string) *gexec.Session {
	return spawn(flyBin, append([]string{"--verbose", "-t", flyTarget}, argv...)...)
}

func spawnFlyInteractive(stdin io.Reader, argv ...string) *gexec.Session {
	return spawnInteractive(stdin, flyBin, append([]string{"-t", flyTarget}, argv...)...)
}

func run(argc string, argv ...string) {
	wait(spawn(argc, argv...))
}

func spawn(argc string, argv ...string) *gexec.Session {
	By("running: " + argc + " " + strings.Join(argv, " "))
	cmd := exec.Command(argc, argv...)
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())
	return session
}

func spawnInteractive(stdin io.Reader, argc string, argv ...string) *gexec.Session {
	By("interactively running: " + argc + " " + strings.Join(argv, " "))
	cmd := exec.Command(argc, argv...)
	cmd.Stdin = stdin
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())
	return session
}

func wait(session *gexec.Session) {
	<-session.Exited
	Expect(session.ExitCode()).To(Equal(0))
}

func waitForLandingOrLandedWorker() string {
	return waitForWorkerInState("landing", "landed")
}

func waitForRunningWorker() string {
	return waitForWorkerInState("running")
}

func waitForStalledWorker() string {
	return waitForWorkerInState("stalled")
}

func waitForWorkerInState(desiredStates ...string) string {
	var workerName string

	Eventually(func() string {
		workers := flyTable("workers")

		for _, worker := range workers {
			name := worker["name"]
			state := worker["state"]

			anyMatched := false
			for _, desiredState := range desiredStates {
				if state == desiredState {
					anyMatched = true
				}
			}

			if !anyMatched {
				continue
			}

			if workerName != "" {
				Fail("multiple workers in states: " + strings.Join(desiredStates, ", "))
			}

			workerName = name
		}

		return workerName
	}).ShouldNot(BeEmpty())

	return workerName
}

func flyTable(argv ...string) []map[string]string {
	session := spawnFly(append([]string{"--print-table-headers"}, argv...)...)
	<-session.Exited
	Expect(session.ExitCode()).To(Equal(0))

	result := []map[string]string{}
	var headers []string

	rows := strings.Split(string(session.Out.Contents()), "\n")
	for i, row := range rows {
		if i == 0 {
			headers = splitFlyColumns(row)
			continue
		}
		if row == "" {
			continue
		}

		result = append(result, map[string]string{})
		columns := splitFlyColumns(row)

		Expect(columns).To(HaveLen(len(headers)))

		for j, header := range headers {
			if header == "" || columns[j] == "" {
				continue
			}

			result[i-1][header] = columns[j]
		}
	}

	return result
}

func splitFlyColumns(row string) []string {
	return regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(row), -1)
}

func waitForWorkersToBeRunning() {
	Eventually(func() bool {
		workers := flyTable("workers")
		anyNotRunning := false
		for _, worker := range workers {

			state := worker["state"]

			if state != "running" {
				anyNotRunning = true
			}
		}

		return anyNotRunning
	}).Should(BeFalse())
}

func workersWithContainers() []string {
	mainTeam := concourseClient().Team("main")
	containers, err := mainTeam.ListContainers(map[string]string{})
	Expect(err).NotTo(HaveOccurred())

	usedWorkers := map[string]struct{}{}

	for _, container := range containers {
		usedWorkers[container.WorkerName] = struct{}{}
	}

	var workerNames []string
	for worker, _ := range usedWorkers {
		workerNames = append(workerNames, worker)
	}

	return workerNames
}

func containersBy(condition, value string) []string {
	containers := flyTable("containers")

	var handles []string
	for _, c := range containers {
		if c[condition] == value {
			handles = append(handles, c["handle"])
		}
	}

	return handles
}

func workersBy(condition, value string) []string {
	containers := flyTable("workers")

	var handles []string
	for _, c := range containers {
		if c[condition] == value {
			handles = append(handles, c["name"])
		}
	}

	return handles
}

func volumesByResourceType(name string) []string {
	volumes := flyTable("volumes", "-d")

	var handles []string
	for _, v := range volumes {
		if v["type"] == "resource" && strings.HasPrefix(v["identifier"], "name:"+name) {
			handles = append(handles, v["handle"])
		}
	}

	return handles
}

// func deleteDeploymentWithForcedDrain() {
// 	delete := spawnBosh("stop")

// 	var workers []string
// 	Eventually(func() []string {
// 		workers = workersBy("state", "retiring")
// 		return workers
// 	}).Should(HaveLen(1))

// 	fly("prune-worker", "-w", workers[0])

// 	<-delete.Exited
// 	Expect(delete.ExitCode()).To(Equal(0))
// }
