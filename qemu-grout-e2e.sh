#!/bin/bash

set -e

ln -s /tmp/kind_logs ./kind_logs.out || true;
t=reports/`date +%Y%m%d%H%M`.out/; 
echo $t;
mkdir -p $t;

export KUBECONFIG=`pwd`/bin/kubeconfig;
rm -rf /tmp/kind_logs || true;
IMG_TAG=main-grout make grout-docker-build qemu-load-image

# Clean up stale CRs before restarting pods to avoid FRR reload failures
# (frr-reload can't execute "no interface u_toswitch1" on active Grout TAPs)
kubectl delete underlays,l3vnis,l2vnis,l3vpns,l3passthroughs,rawfrrconfigs -n openperouter-system --all --ignore-not-found || true;
kubectl delete pod -n openperouter-system -l app=router && kubectl delete pod -n openperouter-system -l app=controller;
kubectl wait --for=condition=Ready pod -n openperouter-system -l app=router --timeout=120s;

set +e

make qemu-e2etests GINKGO_ARGS="-failFast --label-filter=grout-support" TEST_ARGS="--groutmode" > $t/console.out

docker exec -it clab-kind-leafkind1 bash -xc "ip route; ip link; vtysh -c 'show running-config'; cat /etc/frr/frr.log" > /tmp/kind_logs/leafkind1.log;

mv kind_logs.out/* $t; rm reports/latest.out; ln -s ../$t reports/latest.out;


# check folder reports/latest.out. console.log has the test output. subfolders contains logs and details for each test failure. analyze the first test failure in the console.log and create a file analysis.md in that folder with your findings. go deep in the analysis and spawn as many agents as needed to fully analyze the failure.
