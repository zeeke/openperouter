#!/bin/bash

docker exec -it clab-kind-leafkind1 apk add tcpdump
docker exec -it clab-kind-leafkind2 apk add tcpdump
docker exec -it clab-kind-leafSRV6 apk add tcpdump
docker exec -it clab-kind-hostSRV6_red apk add tcpdump
docker exec -it clab-kind-spine apk add tcpdump

pids=()

docker exec -it clab-kind-leafSRV6 tcpdump -vv -enni any > t_leafSRV6.out &
pids+=($!)
docker exec -it clab-kind-hostSRV6_red tcpdump -vv -enni any > t_hostSRV6_red.out &
pids+=($!)
docker exec -it clab-kind-spine tcpdump -vv -enni any > t_spine.out &
pids+=($!)
docker exec -it clab-kind-leafkind1 tcpdump -vv -enni any > t_leafkind1.out &
pids+=($!)
docker exec -it clab-kind-leafkind2 tcpdump -vv -enni any > t_leafkind2.out &
pids+=($!)

cleanup() {
	echo
	echo "Stopping tcpdump captures..."
	for pid in "${pids[@]}"; do
		kill "$pid" 2>/dev/null
	done
	wait "${pids[@]}" 2>/dev/null
	exit 0
}

trap cleanup INT TERM

echo "Capturing traffic. Press Ctrl-C to stop."
wait
