/*
Copyright 2025, Opera Norway AS

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logconfig

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalClient "github.com/operasoftware/cnpg-plugin-pgbackrest/test/e2e/internal/client"
	internalCluster "github.com/operasoftware/cnpg-plugin-pgbackrest/test/e2e/internal/cluster"
	"github.com/operasoftware/cnpg-plugin-pgbackrest/test/e2e/internal/command"
	internalLogs "github.com/operasoftware/cnpg-plugin-pgbackrest/test/e2e/internal/logs"
	nmsp "github.com/operasoftware/cnpg-plugin-pgbackrest/test/e2e/internal/namespace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Configurable pgBackRest log levels", func() {
	var namespace *corev1.Namespace
	var cl client.Client

	BeforeEach(func(ctx SpecContext) {
		var err error
		cl, _, err = internalClient.NewClient()
		Expect(err).NotTo(HaveOccurred())
		namespace, err = nmsp.CreateUniqueNamespace(ctx, cl, "log-config")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(cl.Delete(ctx, namespace)).To(Succeed())
	})

	It("should pass the configured log options to pgBackRest and honor them", func(ctx SpecContext) {
		testResources := createLogConfigTestResources(namespace.Name)
		primaryPod := fmt.Sprintf("%s-1", clusterName)

		By("starting the object store deployment")
		Expect(testResources.ObjectStoreResources.Create(ctx, cl)).To(Succeed())

		By("creating the Archive with an explicit log configuration")
		Expect(cl.Create(ctx, testResources.Archive)).To(Succeed())

		By("creating a CloudNativePG cluster")
		cluster := testResources.Cluster
		Expect(cl.Create(ctx, cluster)).To(Succeed())

		By("having the cluster ready")
		Eventually(func(g Gomega) {
			g.Expect(cl.Get(
				ctx,
				types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
				cluster)).To(Succeed())
			g.Expect(internalCluster.IsReady(*cluster)).To(BeTrue())
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		clientSet, cfg, err := internalClient.NewClientSet()
		Expect(err).NotTo(HaveOccurred())

		By("adding data to PostgreSQL")
		_, _, err = command.ExecuteInContainer(ctx,
			*clientSet,
			cfg,
			command.ContainerLocator{
				NamespaceName: cluster.Namespace,
				PodName:       primaryPod,
				ContainerName: postgresContainer,
			},
			nil,
			[]string{"psql", "-tAc", "CREATE TABLE test (i int); INSERT INTO test VALUES (1);"})
		Expect(err).NotTo(HaveOccurred())

		By("creating a backup")
		// A successful backup also proves that keeping the console level at "off" does not
		// corrupt the JSON emitted by 'pgbackrest info --output=json', which the plugin
		// parses to look up backups. If our extra flags were invalid, pgBackRest would
		// reject the command and the backup would fail.
		backup := testResources.Backup
		Expect(cl.Create(ctx, backup)).To(Succeed())

		By("waiting for the backup to complete")
		Eventually(func(g Gomega) {
			g.Expect(cl.Get(ctx, types.NamespacedName{Name: backup.Name, Namespace: backup.Namespace},
				backup)).To(Succeed())
			g.Expect(backup.Status.Phase).To(BeEquivalentTo(v1.BackupPhaseCompleted))
		}).Within(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("verifying the plugin passed the configured log flags to pgBackRest")
		// The plugin logs each pgBackRest invocation together with its full option list.
		// We assert that at least one such invocation carries every flag derived from the
		// log configuration, with the exact values we set.
		expectedFlags := []string{
			fmt.Sprintf("--log-level-stderr %s", stderrLevel),
			fmt.Sprintf("--log-level-console %s", consoleLevel),
			fmt.Sprintf("--log-level-file %s", fileLevel),
			fmt.Sprintf("--log-path %s", logPath),
		}
		Eventually(func(g Gomega) {
			pluginLogs, logErr := internalLogs.GetPodContainerLogs(
				ctx,
				clientSet,
				cluster.Namespace,
				primaryPod,
				pluginContainer,
				nil,
			)
			g.Expect(logErr).NotTo(HaveOccurred())

			commands := internalLogs.FindPgbackrestCommandOptions(pluginLogs)
			g.Expect(commands).NotTo(BeEmpty(), "expected at least one logged pgBackRest invocation")

			matched := false
			for _, cmd := range commands {
				if containsAll(cmd, expectedFlags) {
					matched = true
					GinkgoWriter.Printf("Found pgBackRest invocation with configured log flags: %s\n", cmd)
					break
				}
			}
			g.Expect(matched).To(BeTrue(),
				"expected a pgBackRest invocation containing all configured log flags %v, got commands: %v",
				expectedFlags, commands)
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("verifying pgBackRest actually wrote debug-level log files to the configured path")
		// The sidecar shares the /controller volume with the postgres container, so the log
		// files pgBackRest writes are visible from the (shell-equipped) postgres container.
		// Their presence proves --log-path was honored; the DEBUG entries prove
		// --log-level-file=debug was honored.
		Eventually(func(g Gomega) {
			listOut, _, listErr := command.ExecuteInContainer(ctx,
				*clientSet,
				cfg,
				command.ContainerLocator{
					NamespaceName: cluster.Namespace,
					PodName:       primaryPod,
					ContainerName: postgresContainer,
				},
				nil,
				[]string{"bash", "-c", fmt.Sprintf("ls -1 %s", logPath)})
			g.Expect(listErr).NotTo(HaveOccurred())
			g.Expect(listOut).To(ContainSubstring(".log"),
				"expected pgBackRest to create log files under %s, got: %q", logPath, listOut)

			catOut, _, catErr := command.ExecuteInContainer(ctx,
				*clientSet,
				cfg,
				command.ContainerLocator{
					NamespaceName: cluster.Namespace,
					PodName:       primaryPod,
					ContainerName: postgresContainer,
				},
				nil,
				[]string{"bash", "-c", fmt.Sprintf("cat %s/*.log", logPath)})
			g.Expect(catErr).NotTo(HaveOccurred())
			g.Expect(catOut).To(ContainSubstring("DEBUG"),
				"expected pgBackRest log files to contain DEBUG entries when log-level-file is set to %q", fileLevel)
		}).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
	})
})

// containsAll reports whether s contains every one of the provided substrings.
func containsAll(s string, substrings []string) bool {
	for _, substr := range substrings {
		if !strings.Contains(s, substr) {
			return false
		}
	}
	return true
}
