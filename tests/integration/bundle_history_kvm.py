#!/usr/bin/env python3
"""Exercise two managed Bundle readers and an archived Serve on one KVM Worker."""

import argparse
import grp
import json
import os
from pathlib import Path
import select
import signal
import shutil
import struct
import socket
import sqlite3
import subprocess
import sys
import tempfile
from contextlib import ExitStack, closing
from http.client import HTTPConnection

ROOT = Path(__file__).resolve().parents[2]
HISTORY = ROOT / "packaging/deep/bundle-history.py"


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("smolvm", "prior", "candidate", "archived", "platformd", "guest"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--local-child", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("the isolated test requires root and KVM")
    return args


def start(stack, args, env, log):
    target = stack.enter_context(log.open('ab'))
    process = subprocess.Popen(args, env=env, stdin=subprocess.DEVNULL, stdout=target, stderr=target)
    def stop():
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    stack.callback(stop)
    return process


def handshake(bundle, endpoint):
    manifest_file = bundle / 'runtime-bundle.json'
    manifest = json.loads(manifest_file.read_text())
    h = until(lambda: http(endpoint, EXT + '/capabilities'), 15)
    assert h['bundle_digest'] == manifest['bundle_digest'] and h['state_reader_id'] == manifest['contracts']['state_reader_id'], h
    return dict(instance_id=h['runtime_instance_id'], bundle_digest=h['bundle_digest'], state_reader_id=h['state_reader_id'],
                socket=str(endpoint), manifest=str(manifest_file))


def scope(value):
    return {name: value[name] for name in ('instance_id', 'incarnation_id', 'execution_epoch')}


def op(endpoint, key, kind, expected, params):
    accepted = http(endpoint, EXT + '/operations', dict(operation_id=key, kind=kind, expected=scope(expected),
                    request_digest=digest(key.encode()), parameters=params))
    assert accepted['phase'] == 'NEW', accepted
    def status():
        receipt = http(endpoint, EXT + '/operations/' + key)
        assert receipt['phase'] != 'ABORTED', receipt
        return receipt if receipt['phase'] == 'COMPLETED' else None
    return until(status, 150)


def records(db, query, args=()):
    with closing(sqlite3.connect(db)) as con:
        return con.execute(query, args).fetchall()


def exercise(args, endpoint, root, source, inventory, _):
    subprocess.run(['ip', 'link', 'set', 'lo', 'up'], check=True)
    worker = root / 'ipc/worker.sock'
    catalog = root / 'catalog/catalog.sqlite'
    env = dict(os.environ, HOME=str(root / 'home'), RUST_LOG='error')
    with ExitStack() as stack:
        def launch_serve(bundle, name, port):
            path = root / (name + '/s.sock')
            flags = ['--worker-resource-socket', worker] if name != 'archived' else []
            process = start(stack, [bundle / 'smolvm-bin', '--state-dir', root / (name + '-state'), 'serve', 'start',
                            '--listen', f'unix://{path}', '--guest-rollout-port', str(port), '--shutdown-grace', '0', *flags],
                            dict(env, SMOLVM_LIB_DIR=str(bundle / 'lib'), LD_LIBRARY_PATH=str(bundle / 'lib')), root / (name + '.log'))
            return process, path, handshake(bundle, path)

        old = handshake(root / 'bundle', endpoint)
        staged_candidate = root / 'candidate-bundle'
        shutil.copytree(args.candidate, staged_candidate, symlinks=True)
        new_process, new_endpoint, new = launch_serve(staged_candidate, 'candidate', 37227)
        old_process, old_endpoint, archived = launch_serve(args.archived, 'archived', 37228)
        assert len({route['instance_id'] for route in (old, new, archived)}) == 3
        assert len({route['bundle_digest'] for route in (old, new, archived)}) == 3
        print('Verified three distinct persistent Serve identities', old['instance_id'], new['instance_id'], archived['instance_id'], flush=True)
        unused = http(new_endpoint, EXT + '/machines/history-new/binding')
        wrong_attach = dict(operation_id='conflicting-volume', request_digest=digest(b'foreign lease'),
            volume_id='product-storage', owner_id='volume-test', expected_revision=2,
            command=dict(action='attach', attachment_id='attach-foreign', instance_id=unused['instance_id'],
                         execution_epoch=0, device_key='storage', mode='rw', fork_policy='clone', capture_policy='include'))
        actual = http(endpoint, EXT + '/volumes/product-storage?owner_id=volume-test')
        wrong_attach['expected_revision'] = actual['revision']
        try:
            http(new_endpoint, EXT + '/volumes', wrong_attach)
        except RuntimeError as error:
            assert any(code in str(error) for code in ('E_VOLUME_BUSY', 'E_NOT_FOUND')), error
        else:
            raise AssertionError('another Bundle acquired a running writer')
        assert http(endpoint, EXT + '/volumes/product-storage?owner_id=volume-test') == actual
        print('A second Bundle was refused the running writer in the same Worker catalog', flush=True)
        rpc(worker, 'RegisterPrivateNetwork', dict(network_id='shared-history-pool', route_domain_id='history-router', cidr='10.86.0.0/30'))
        reservations = [dict(route_id='deep', bundle_digest=route['bundle_digest'], operation_id=f'history-net-{index}',
                             request_digest=digest(f'history-net-{index}'.encode()), expected=dict(instance_id=f'history-network-{index}',
                             incarnation_id=None, execution_epoch=0)) for index, route in enumerate((old,new))]
        for request in reservations:
            rpc(worker, 'BeginOperation', dict(kind='network_reservation'), **request)
        net = dict(attachment_id='history-network-first', network_id='shared-history-pool', device_id='net0', mode='native')
        address = rpc(worker, 'ReserveNetworkAttachment', net, **reservations[0])
        try:
            rpc(worker, 'ReserveNetworkAttachment', {**net, 'attachment_id':'history-network-second'}, **reservations[1])
        except RuntimeError as error:
            assert 'E_RESOURCE_EXHAUSTED' in str(error), error
        else:
            raise AssertionError('two Bundles reserved one route-domain address')
        rpc(worker, 'ReleaseNetworkReservation', dict(attachment_id=net['attachment_id'], expected_revision=address['revision']), **reservations[0])
        for request in reservations:
            rpc(worker, 'FinishOperation', dict(abort=False, expected_revision=1), **request)
        print('Worker allocator refused the second Bundle the only address in the shared pool', flush=True)
        disks = [dict(attachment_id='attach-' + name, device_id=name, role=name, generation_id=generation, read_only=ro)
                 for name, generation, ro in (('root','base-ext4',True),('storage','empty-data',False),('data','empty-data',False))]
        other = op(new_endpoint, 'new-history-cold', 'launch', unused, dict(name='history-new', boot_profile='oci-v1', cpus=2,
                   memory_mib=512, disks=disks, init='/counter', args=[]))['execution']
        other_log = root / 'candidate-state/managed' / (other['incarnation_id'] + '.log')
        until(lambda: other_log.is_file() and 'PARKED_BLOCK_TICK' in other_log.read_text(errors='replace'), 25)
        other_vm = http(new_endpoint, EXT + f"/instances/{other['instance_id']}/inventory")['execution']
        print('Two Bundle-scoped KVM executions coexist in one Worker', inventory['execution']['pid'], other_vm['pid'], flush=True)
        print('VMM scopes', (Path('/proc') / str(inventory['execution']['pid']) / 'cgroup').read_text().strip(),
              (Path('/proc') / str(other_vm['pid']) / 'cgroup').read_text().strip(), flush=True)
        point = op(endpoint, 'history-snapshot', 'vm_snapshot', source, {})['statepoint']
        assert point['phase'] == 'durable' and point['source_released']
        foreign = http(new_endpoint, EXT + '/machines/wrong-reader/binding')
        failure = None
        try:
            op(new_endpoint, 'history-foreign-restore', 'vm_restore', foreign,
               dict(name='wrong-reader', capture_id=point['capture_id'], mode='clone_conflicts'))
        except (RuntimeError, AssertionError) as error:
            failure = str(error)
            assert any(marker in failure for marker in ('BUNDLE', 'bundle', 'reader', 'capture scope', 'not available')), failure
        assert failure is not None, 'mismatched Bundle read a historical capture'
        print('Durable snapshot', point['manifest_digest'], 'was rejected by the other bundle without changing the source', flush=True)

        config = root / 'platform.json'
        passwd = root / 'platform-password'
        password = os.urandom(20).hex()
        passwd.write_text(password + '\n')
        passwd.chmod(0o600)
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        config.write_text(json.dumps(dict(version=1, state_dir=str(root / 'platform'),
                api=dict(listen=f'127.0.0.1:{port}', public_origin=f'http://127.0.0.1:{port}'),
                runtime=dict(socket=str(root / 'ipc/official.sock'), data_dir=str(root / 'official'), status_socket='',
                             custom=[old, new, archived]))))
        registry = root / 'platform/platform.db'
        def start_platform():
            process = start(stack, [args.platformd, '--config', config],
                    dict(env, SANDBOX_OWNER_USER='owner', SANDBOX_OWNER_PASSWORD_FILE=str(passwd)), root / 'platform.log')
            def listening():
                assert process.poll() is None, (root / 'platform.log').read_text()
                with socket.socket() as client:
                    return client.connect_ex(('127.0.0.1', port)) == 0
            until(listening, 20)
            return process
        def profiles():
            client = HTTPConnection('127.0.0.1', port, timeout=8)
            headers = {'Origin': f'http://127.0.0.1:{port}', 'Content-Type': 'application/json'}
            try:
                client.request('POST', '/v1/auth/login', json.dumps(dict(username='owner', password=password)), headers)
                login = client.getresponse()
                login.read()
                assert login.status == 200, login.status
                headers['Cookie'] = login.getheader('Set-Cookie').split(';', 1)[0]
                client.request('GET', '/v1/runtime-profiles', headers=headers)
                response = client.getresponse()
                assert response.status == 200, response.status
                return {row['runtime_bundle_id']: row for row in json.loads(response.read())['items']}
            finally:
                client.close()
        app = start_platform()
        assert len(records(registry, 'SELECT id FROM runtime_instances')) == 3
        assert len(profiles()) == 3
        ref = subprocess.run([sys.executable, HISTORY, 'references', '--registry', registry, '--worker-catalog', catalog,
                              '--bundle', old['bundle_digest']], check=True, capture_output=True, text=True)
        references = json.loads(ref.stdout)
        assert references['worker']['durable_or_exporting_statepoints'] == 1 and not references['can_retire'], references
        assert references['platform']['registered_routes'] == 1, references
        print('Live Worker and Platform reference inspection pinned the old Bundle', references, flush=True)

        old_process.terminate()
        old_process.wait(timeout=10)
        _, _, same_archive = launch_serve(args.archived, 'archived', 37228)
        assert same_archive == archived
        new_process.terminate()
        new_process.wait(timeout=10)
        _, _, same_candidate = launch_serve(staged_candidate, 'candidate', 37227)
        assert same_candidate == new
        assert http(new_endpoint, EXT + '/instances/' + other['instance_id'] + '/inventory')['execution']['pid'] == other_vm['pid']
        assert http(endpoint, EXT + '/instances/' + source['instance_id'] + '/inventory')['execution']['pid'] == inventory['execution']['pid']
        app.terminate()
        app.wait(timeout=10)
        subprocess.run([sys.executable, HISTORY, 'set-security', '--registry', registry, '--bundle', archived['bundle_digest'],
                        '--status', 'quarantined', '--reason', 'isolated recovery fixture'], check=True)
        app = start_platform()
        reporting = profiles()[archived['bundle_digest']]
        assert reporting['security_status'] == 'quarantined' and reporting['security_reason'] == 'isolated recovery fixture', reporting
        app.terminate()
        app.wait(timeout=10)
        subprocess.run([sys.executable, HISTORY, 'set-security', '--registry', registry, '--bundle', archived['bundle_digest'],
                        '--status', 'legacy-allowed', '--reason', 'rollback selection fixture'], check=True)
        app = start_platform()
        assert records(registry, 'SELECT security_status FROM runtime_bundles WHERE id=?', (archived['bundle_digest'],)) == [('legacy-allowed',)]
        assert http(new_endpoint, EXT + '/instances/' + other['instance_id'] + '/inventory')['execution']['pid'] == other_vm['pid']
        print('Reopened the archived Serve from its original state, restored legacy route eligibility, and kept both KVM executions', flush=True)

        with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as client:
            client.connect(str(worker))
            pid, _, _ = struct.unpack('3i', client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        handle = os.pidfd_open(pid)
        try:
            signal.pidfd_send_signal(handle, signal.SIGKILL)
            assert select.select([handle], [], [], 10)[0], 'Worker did not exit'
        finally:
            os.close(handle)
        start(stack, [root / 'bundle/smolvm-resourced', 'serve', '--socket', worker, '--state-dir', root / 'catalog',
                      '--socket-gid', str(grp.getgrnam('kvm').gr_gid)], env, root / 'worker-restart.log')
        until(lambda: worker_ready(worker), 20)
        assert records(catalog, 'SELECT manifest_digest FROM statepoints WHERE capture_id=?', (point['capture_id'],)) == [(point['manifest_digest'],)]
        worker_health = rpc(worker, 'Health')
        assert worker_health['state'] == 'recovering'
        for route, binding, execution in ((old, source, inventory['execution']), (new, other, other_vm)):
            handle = os.pidfd_open(execution['pid'])
            try:
                stat = (Path('/proc') / str(execution['pid']) / 'stat').read_text().rsplit(')', 1)[1].split()
                assert int(stat[19]) == execution['pid_start_time']
                signal.pidfd_send_signal(handle, signal.SIGKILL)
                assert select.select([handle], [], [], 10)[0]
            finally:
                os.close(handle)
            until(lambda: not (Path('/proc') / str(execution['pid']) / 'stat').exists(), 15)
            final = rpc(worker, 'FinalizeExecutionExit', route_id='deep', bundle_digest=route['bundle_digest'], expected=scope(binding))
            assert final['phase'] == 'exited', final
        status = rpc(worker, 'CompleteRecovery', dict(broker_session_id=worker_health['broker_session_id']))
        assert status['state'] == 'ready', status
        print('Broker waited for process and OFD fences. Verified old data survived closure of both original executions.', flush=True)
        recovered = http(endpoint, EXT + '/machines/history-original-reader/binding')
        restored = op(endpoint, 'history-original-reader', 'vm_restore', recovered,
                dict(name='history-original-reader', capture_id=point['capture_id'], mode='clone_conflicts'))['statepoint']
        assert restored['manifest_digest'] == point['manifest_digest'] and restored['activation_decision'] == 'commit'
        print('Original reader restored the retained checkpoint after the shared Worker restart', flush=True)

def main(args):
    if not args.local_child:
        command = ["unshare", "--mount", "--propagation", "private", "--net", "--", sys.executable,
                   __file__, *sys.argv[1:], "--local-child"]
        subprocess.run(command, check=True)
        return
    with tempfile.TemporaryDirectory(prefix="history-kvm-", dir="/var/tmp") as directory:
        root = Path(directory)
        root.chmod(0o755)
        try:
            verify(args.prior, root, args.guest, None, False, True, False, False,
                   observer=lambda *context: exercise(args, *context))
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2400:], file=sys.stderr)
            raise


if __name__ == "__main__":
    args = parse_args()
    sys.path.insert(0, str(args.smolvm / "tests"))
    from test_deep_managed_launch import EXT, verify, http, until, worker_ready, digest, rpc

    main(args)
