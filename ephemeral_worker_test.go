package topgun_test

import (
	_ "github.com/lib/pq"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = FDescribe("Ephemeral Workers", func() {
	Context("with an ephemeral worker available", func() {
		BeforeEach(func() {
			Deploy("deployments/concourse-separate-forwarded-worker.yml", "-o", "operations/separate-worker-two.yml", "-o", "operations/ephemeral-worker.yml")
		})

		Context("when the worker goes away", func() {
			BeforeEach(func() {
				bosh("ssh", "worker/0", "-c", "sudo /var/vcap/bosh/bin/monit stop worker")
				bosh("ssh", "worker/0", "-c", "sudo /var/vcap/bosh/bin/monit stop garden")
			})

			It("disappears without stalling", func() {
				Eventually(func() int {
					workers := flyTable("workers")
					return len(workers)
				}).Should(Equal(1))
			})
		})
	})
})
