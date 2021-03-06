// ebpf_exporter - A Prometheus exporter for Linux block IO statistics.
//
// Copyright 2018 Daniel Swarbrick
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

const bpfSource string = `
#include <uapi/linux/ptrace.h>
#include <linux/blkdev.h>
#include <linux/blk_types.h>

typedef struct disk_key {
	char disk[DISK_NAME_LEN];	// 32 bytes
	u8 req_op;
	u64 slot;
} disk_key_t;				// 48 bytes, with padding

const u8 max_io_lat_slot = 28;		// log2 range 1 us to ~2 mins
const u8 max_io_req_sz_slot = 16;	// log2 range 1 KiB to 32 MiB

// Hash to temporily hold the start time of each bio request - macro for
// BPF_TABLE("hash", _key_type, u64, _name, 10240). Increase if you expect
// more than 10K IO requests in flight.
BPF_HASH(start, struct request *);

// Histograms to hold IO request latency / size bucket values - macro for
// BPF_TABLE("histogram", _key_type, u64, _name, _size). Total number of
// buckets are shared amongst all devices and all request operation types.
// Unlike Prometheus histograms, these are sparse, so will only use a bucket
// if required. Since most request operations will be read or write, a good
// rule of thumb is: num_devices * 2 req_op types * 20 buckets each. Bear in
// mind that the amount of memory used will be (sizeof(_key_type) +
// sizeof(u64)) * _size, so the following will use 560 KiB each.
BPF_HISTOGRAM(io_lat, disk_key_t, 10240);
BPF_HISTOGRAM(io_req_sz, disk_key_t, 10240);

// Record start time of a request
int trace_req_start(struct pt_regs *ctx, struct request *req)
{
	u64 ts = bpf_ktime_get_ns();
	start.update(&req, &ts);
	return 0;
}

// Calculate request duration and store in appropriate histogram bucket
int trace_req_completion(struct pt_regs *ctx, struct request *req, unsigned int bytes)
{
	u64 *tsp, delta, slot;
	u8 req_op;

	// Fetch timestamp and calculate delta
	tsp = start.lookup(&req);
	if (tsp == 0) {
		return 0;   // missed issue
	}

	// Request duration, in microseconds
	delta = (bpf_ktime_get_ns() - *tsp) / 1000;

	// Request operation, e.g. REQ_OP_READ, REQ_OP_WRITE, etc.
	req_op = req->cmd_flags & REQ_OP_MASK;

	// Latency histogram key
	slot = bpf_log2l(delta);
	if (slot >= max_io_lat_slot)
		slot = max_io_lat_slot - 1;
	disk_key_t lat_key = {.slot = slot, .req_op = req_op};
	bpf_probe_read(&lat_key.disk, sizeof(lat_key.disk), req->rq_disk->disk_name);

	// Request size histogram key
	slot = bpf_log2(bytes / 1024);
	if (slot >= max_io_req_sz_slot)
		slot = max_io_req_sz_slot - 1;
	disk_key_t req_sz_key = {.slot = slot, .req_op = req_op};
	bpf_probe_read(&req_sz_key.disk, sizeof(req_sz_key.disk), req->rq_disk->disk_name);

	io_lat.increment(lat_key);
	io_req_sz.increment(req_sz_key);

	start.delete(&req);
	return 0;
}
`
