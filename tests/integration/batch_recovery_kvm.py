#!/usr/bin/env python3
"""Show best-effort admission and live sealed RAM/OFD recovery through the public platform."""
import argparse
import grp
import json
import os
from pathlib import Path
import select
import signal
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
from contextlib import ExitStack, closing
from http.client import HTTPConnection

def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("smolvm", "bundle", "platformd", "guest"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--local-child", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("the isolated test requires root and KVM")
    return args


def query(db, statement, args=()):
    with closing(sqlite3.connect(db)) as connection:
        return connection.execute(statement, args).fetchall()

def terminate_vm(record):
    pid = record['pid']
    descriptor = os.pidfd_open(pid)
    try:
        fields = (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()
        assert int(fields[19]) == record['pid_start_time']
        signal.pidfd_send_signal(descriptor, signal.SIGKILL)
        assert select.select([descriptor], [], [], 10)[0]
    finally:
        os.close(descriptor)

def start(stack, command, env, log):
    dest = stack.enter_context(log.open('ab'))
    process = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL, stdout=dest, stderr=dest)
    def stop():
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=6)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    stack.callback(stop)
    return process

def tick(path, hostname=None):
    if not path.exists():
        return None
    rows = [line.split() for line in path.read_text(errors='replace').splitlines() if line.startswith('PARKED_BLOCK_TICK ')]
    rows = [row for row in rows if len(row) == 5 and (hostname is None or row[3] == hostname)]
    return rows[-1] if rows else None

def verify_product(args, endpoint, root, source, inventory, _):
    subprocess.run(['ip', 'link', 'set', 'lo', 'up'], check=True)
    worker = root / 'ipc/worker.sock'
    database = root / 'catalog/catalog.sqlite'
    state = root / 'platform'
    installed = root / 'bundle'
    caps = http(endpoint, EXT + '/capabilities')
    route = dict(instance_id=caps['runtime_instance_id'], bundle_digest=caps['bundle_digest'],
                 state_reader_id=caps['state_reader_id'], socket=str(endpoint), manifest=str(installed / 'runtime-bundle.json'))
    assert rpc(worker, 'QueryWorkerBudget')['capacity']['vcpus'] == 4
    assert query(database, 'PRAGMA user_version') == [(20,)]
    with socket.socket() as slot:
        slot.bind(('127.0.0.1', 0))
        port = slot.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    config, secret = root / 'platform.json', root / 'owner-password'
    password = os.urandom(24).hex()
    secret.write_text(password + '\n')
    secret.chmod(0o600)
    config.write_text(json.dumps(dict(version=1, state_dir=str(state), api=dict(listen=f'127.0.0.1:{port}', public_origin=origin),
        runtime=dict(socket=str(root/'ipc/official.sock'), data_dir=str(root/'official'), status_socket='', custom=[route]))))
    token = csrf = ''
    with ExitStack() as stack:
        start(stack, [args.platformd, '--config', config], dict(os.environ, SANDBOX_OWNER_PASSWORD_FILE=str(secret)), root/'platform.log')
        def api(path, body=None, headers=None, status=200):
            headers = {'Origin': origin, 'Content-Type': 'application/json', **(headers or {})}
            if token:
                headers.update({'Cookie': token, 'X-CSRF-Token': csrf})
            connection = HTTPConnection('127.0.0.1', port, timeout=10)
            try:
                connection.request('GET' if body is None else 'POST', '/v1/' + path,
                    None if body is None else json.dumps(body), headers)
                reply = connection.getresponse()
                data = json.loads(reply.read())
                assert reply.status == status, (path, reply.status, data)
                if path == 'auth/login':
                    return data, reply.getheader('Set-Cookie').split(';', 1)[0]
                return data
            finally:
                connection.close()
        session, token = until(lambda:api('auth/login', dict(username='owner', password=password)), 20)
        csrf = session['csrf_token']
        epoch = api('control')['control_epoch']
        with sqlite3.connect(state/'platform.db') as db:
            db.execute('PRAGMA foreign_keys=ON')
            db.execute("INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES('counter','OCI counter',1,'sha256:counter','{}','{}')")
            db.execute("INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running) VALUES('testsource','OCI source','counter',1,1,'running','{}','{}',1)")
            db.execute('''INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,
                runtime_instance_id,runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
                VALUES('sourcebinding','testsource',1,'test-managed','current','sourcenamespace','fixture',?,?,?,?,?,'running')''',
                (route['instance_id'],route['bundle_digest'],source['instance_id'],source['incarnation_id'],source['execution_epoch']))
            db.execute("UPDATE machines SET current_binding_id='sourcebinding' WHERE id='testsource'")
        def forkable(machine):
            info = api('machines/' + machine)
            return info if info['capabilities']['state.fork_batch']['status'] == 'supported' else None
        def submit(machine, key, names, admission=None):
            info = until(lambda: forkable(machine), 50)
            body = dict(expected_generation=info['binding_generation'], children=[dict(name=name) for name in names], require_independent=True)
            if admission is not None:
                body['admission'] = admission
            headers = {'If-Control-Epoch': epoch, 'If-Match':f'"{info["revision"]}"', 'Idempotency-Key':key}
            path = 'machines/' + machine + '/fork-batches'
            return api(path, body, headers, 202), path, body, headers
        def settled(initial):
            path = 'operations/' + initial['operation_id']
            def ready():
                current = api(path)
                return current if current['state'] in ('succeeded','failed','cancelled','recovery_required') else None
            return until(ready, 155)
        denied, *_ = submit('testsource', 'all-or-reject-fixture', ['Atomic target one', 'Atomic target two'])
        denied = settled(denied)
        assert denied['state'] == 'failed' and denied['effect'] == 'none' and denied['error']['code'] == 'RESOURCE_LIMIT', denied
        assert query(database,'SELECT COUNT(*) FROM managed_fork_batches WHERE operation_id=?', (denied['operation_id'],)) == [(0,)]
        assert rpc(worker, 'QueryWorkerBudget')['allocated']['vcpus'] == 2
        first, path, body, headers = submit('testsource', 'best-effort-fixture', ['Admitted target', 'Over limit target'], 'best_effort')
        result = settled(first)
        assert api(path, body, headers, 202)['operation_id'] == first['operation_id']
        assert result['state'] == 'succeeded' and result['effect'] == 'partial', result
        approved, refused = result['children']
        assert [child['machine_id'] for child in first['children']] == [approved['machine_id'], refused['machine_id']]
        assert result['result']['admission'] == 'best_effort' and result['result']['admitted_child_ids'] == [approved['machine_id']], result
        assert approved['state'] == 'ready' and refused['state'] == 'quarantined' and refused['error_code'] == 'RESOURCE_LIMIT', result
        receipt = http(endpoint, EXT + '/operations/' + result['operation_id'])['fork']
        assert receipt['children'][1]['phase'] == 'ABORTED' and receipt['children'][1]['admission_error'] == 'E_RESOURCE_LIMIT', receipt
        assert rpc(worker, 'QueryWorkerBudget')['allocated']['vcpus'] == 4
        print('Public batch default rolled back atomically. Explicit best_effort kept one stable admitted child and one capacity refusal.', flush=True)

        child_slot = receipt['children'][0]
        child_vm = http(endpoint, EXT + f"/instances/{child_slot['instance_id']}/inventory")['execution']
        child_log = root / 'state/managed' / (child_slot['incarnation_id'] + '.log')
        before = until(lambda: tick(child_log), 20)
        capture = receipt['capture_id']
        identities = query(database, '''SELECT b.identities_json FROM fd_transfer_batches b JOIN fd_transfers t USING(transfer_id)
                        WHERE t.resource_id=? ORDER BY b.batch_index''', (capture,))
        original_lease = query(database, "SELECT lease_id,state,revision FROM fd_leases WHERE incarnation_id=? AND resource_id=?",
                              (child_slot['incarnation_id'],capture))
        assert identities and len(original_lease) == 1 and original_lease[0][1] == 'attached', original_lease
        terminate_vm(inventory['execution'])
        until(lambda: http(endpoint, EXT + '/machines/test-managed/binding')['incarnation_id'] is None, 20)
        initial_session = rpc(worker, 'Health')['broker_session_id']
        with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
            connection.connect(str(worker))
            worker_pid, _, _ = struct.unpack('3i', connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        old = os.pidfd_open(worker_pid)
        try:
            signal.pidfd_send_signal(old, signal.SIGKILL)
            assert select.select([old], [], [], 10)[0]
        finally:
            os.close(old)
        start(stack, [installed/'smolvm-resourced', 'serve', '--socket', worker, '--state-dir', root/'catalog',
                      '--socket-gid', str(grp.getgrnam('kvm').gr_gid)], os.environ, root/'worker-restart.log')
        until(lambda: worker_ready(worker), 20)
        until(lambda: rpc(worker, 'Health')['state']=='ready', 30)
        assert rpc(worker, 'Health')['broker_session_id'] != initial_session
        recovered_vm = http(endpoint, EXT + f"/instances/{child_slot['instance_id']}/inventory")['execution']
        assert recovered_vm['pid'] == child_vm['pid'] and recovered_vm['pid_start_time'] == child_vm['pid_start_time']
        assert query(database, "SELECT state FROM fd_transfers WHERE resource_id=?", (capture,)) == [('committed',)]
        assert query(database, "SELECT state FROM resources WHERE resource_id=?", (capture,)) == [('available',)]
        assert query(database, "SELECT state,revision FROM fd_leases WHERE lease_id=?", (original_lease[0][0],)) == [('attached',original_lease[0][2]+2)]
        assert query(database, "SELECT state FROM writer_grants WHERE incarnation_id=? AND state!='released'", (child_slot['incarnation_id'],)) == [('attached',)]
        after = until(lambda: tick(child_log), 10)
        assert int(after[1]) > int(before[1]) and after[2] == before[2]
        print('Broker restart reattached both writable heads and exact sealed RAM in the original live child after the source exited.', flush=True)

        grandson, *_ = submit(approved['machine_id'], 'after-worker-recovery', ['Independent descendant'])
        grandson = settled(grandson)
        assert grandson['state'] == 'succeeded' and grandson['effect'] == 'applied', grandson
        grand_receipt = http(endpoint, EXT + '/operations/' + grandson['operation_id'])['fork']['children'][0]
        grand_log = root/'state/managed'/(grand_receipt['incarnation_id']+'.log')
        descendant = until(lambda:tick(grand_log), 25)
        assert descendant[2] == before[2] and int(descendant[1]) > int(after[1])
        print('Public fork after Worker recovery continued the original Guest PID and verified both writable disks in an independent descendant.', flush=True)

def main(args):
    if not args.local_child:
        command = ["unshare", "--mount", "--propagation", "private", "--net", "--", sys.executable,
                   __file__, *sys.argv[1:], "--local-child"]
        subprocess.run(command, check=True)
        return
    os.environ['SMOLVM_WORKER_VCPU_BUDGET'] = '4'
    with tempfile.TemporaryDirectory(prefix='provider-kvm-', dir='/var/tmp') as folder:
        root = Path(folder)
        root.chmod(0o755)
        try:
            verify(args.bundle, root, args.guest, None, False, False, False, False,
                   observer=lambda *context: verify_product(args, *context))
        except BaseException:
            for log in [*root.glob('*.log'), *sorted((root/'state/managed').glob('*.log'))]:
                raw = log.read_text(errors='replace')
                print(log.name, raw[:900] + '\n…\n' + raw[-2200:], file=sys.stderr)
            if (root/'catalog/catalog.sqlite').is_file():
                print('escrow', query(root/'catalog/catalog.sqlite', 'SELECT resource_id,state FROM fd_transfers'), file=sys.stderr)
                print('grants', query(root/'catalog/catalog.sqlite', 'SELECT grant_id,state FROM writer_grants'), file=sys.stderr)
            raise


if __name__ == "__main__":
    args = parse_args()
    sys.path.insert(0, str(args.smolvm / "tests"))
    from test_deep_managed_launch import EXT, verify, http, until, worker_ready, rpc

    main(args)
