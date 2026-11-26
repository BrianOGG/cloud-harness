#!/usr/bin/env python3
"""Prepare or exercise the dedicated Agnes serve/Manager acceptance.

Default: print a blocked plan, without HTTP, credential access, or file writes.
--prepare writes a key-free config/workspace. --run makes real model requests
through an already running, separately authorized serve process.
This script never reads a credential or calls the upstream API directly.
"""

import argparse
import hashlib
import json
import platform
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

RUNTIME = Path('/home/ubuntu/cloud-harness-dev/previews/cp1-agnes-868173b-20261126')
ORIGIN = 'http://127.0.0.1:19437'
UPSTREAM = 'https://apihub.agnes-ai.com/v1'
MODEL = 'agnes-3.0-flash'
FILENAME = 'agnes-acceptance.txt'
MARKER = 'agnes application approval acceptance\n'
BLOCKER = ('Review artifact only: live acceptance is not executed by default. '
           'A separately authorized operator must start and verify the isolated '
           'application before selecting --run. Account billing remains unverified.')
CONFIG = {
    'listen': '127.0.0.1:19437', 'db': str(RUNTIME / 'harness.db'),
    'provider': 'openai', 'openai_base_url': UPSTREAM, 'openai_model': MODEL,
    'openai_header_timeout_ms': 360000, 'openai_idle_timeout_ms': 360000,
    'model_profiles': {}, 'model_roles': {}, 'model_prices': {},
    'servers': [], 'notify': {'channels': []},
}


class AcceptanceError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise AcceptanceError(message)


def now():
    return datetime.now(timezone(timedelta(hours=8), 'Asia/Shanghai')).isoformat()


def write_json(path, value, mode='w'):
    with path.open(mode, encoding='utf-8') as target:
        json.dump(value, target, ensure_ascii=False, indent=2)
        target.write('\n')


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Application:
    def __init__(self, timeout, evidence):
        self.timeout = timeout
        self.evidence = evidence
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, route, body=None, expected=200):
        require(route.startswith('/') and not route.startswith('//'), 'Invalid local route')
        record = {'at_shanghai': now(), 'method': method, 'route': route}
        self.evidence['http'].append(record)
        self.save()  # Retain long attempts; never retry a model request.
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(ORIGIN + route, data=data, method=method)
        if data is not None:
            request.add_header('Content-Type', 'application/json')
        started = time.monotonic()
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                record['http_status'] = response.status
                require(response.status == expected, 'Unexpected application HTTP status')
                value = json.load(response)
        except urllib.error.HTTPError as error:
            record['http_status'] = error.code
            raise AcceptanceError(f'Application HTTP {error.code}; body omitted') from None
        finally:
            record['elapsed_seconds'] = round(time.monotonic() - started, 3)
            self.save()
        return value

    def save(self):
        write_json(RUNTIME / 'evidence.json', self.evidence)

    def messages(self, session):
        messages, cursor, cursors, ids = [], '', set(), set()
        while True:
            query = urllib.parse.urlencode({'limit': 200, 'cursor': cursor})
            page = self.request('GET', f'/v1/sessions/{session}/messages?{query}')
            require(isinstance(page.get('messages'), list), 'Missing message array')
            require(isinstance(page.get('next_cursor'), str), 'Missing message cursor')
            for message in page['messages']:
                require(message['session_id'] == session, 'Cross-session message')
                require(message['id'] not in ids, 'Duplicate persisted message ID')
                ids.add(message['id'])
                messages.append(message)
            cursor = page['next_cursor']
            if not cursor:
                break
            require(cursor not in cursors, 'Repeated message cursor')
            cursors.add(cursor)
        keys = [(item['created_at'], item['id']) for item in messages]
        require(keys == sorted(keys), 'Transcript order changed')
        return messages


def rows(db, sql, args=()):
    return [dict(row) for row in db.execute(sql, args)]


def responses(db, session):
    result = rows(db, '''SELECT m.id, m.run_id, m.provider, m.provider_response_id,
        m.complete, m.usage_json FROM model_responses m
        JOIN runs r ON r.id=m.run_id WHERE r.session_id=? ORDER BY m.created_at,m.id''',
        (session,))
    require(result, 'No persisted model response')
    for response in result:
        usage = json.loads(response.pop('usage_json'))
        require(response['provider'] == 'openai' and response['complete'] == 1,
                'Incomplete or different-provider response')
        require(usage.get('model') == MODEL, 'Persisted model differs from Agnes target')
        response['usage'] = usage
    require(not rows(db, '''SELECT e.seq FROM events e JOIN runs r ON r.id=e.run_id
        WHERE r.session_id=? AND e.type='provider.fallback' ''', (session,)),
        'Model fallback observed')
    require(not rows(db, "SELECT id FROM runs WHERE session_id=? AND parent_run_id<>''",
                     (session,)), 'Unexpected child run')
    return result


def verify_pending(db, run, invocation):
    proof = rows(db, '''SELECT a.id AS approval_id,a.state,i.id AS invocation_id,
        i.tool_name,i.status,i.dispatched,i.provider_call_id,b.model_response_id,
        m.provider,m.complete,m.usage_json FROM approvals a
        JOIN tool_invocations i ON i.id=a.invocation_id AND i.run_id=a.run_id
        JOIN tool_batches b ON b.batch_id=i.batch_id
        JOIN model_responses m ON m.id=b.model_response_id AND m.run_id=i.run_id
        JOIN model_response_items item ON item.response_id=m.id
          AND item.provider_call_id=i.provider_call_id AND item.kind='tool_call'
        WHERE a.run_id=? AND a.invocation_id=?''', (run, invocation))
    require(len(proof) == 1, 'No unique persisted model/tool/approval source link')
    proof = proof[0]
    usage = json.loads(proof.pop('usage_json'))
    require(proof['provider'] == 'openai' and proof['complete'] == 1
            and usage.get('model') == MODEL, 'Approval lacks target model provenance')
    require(proof['state'] == 'pending' and proof['status'] == 'WAITING_APPROVAL'
            and proof['dispatched'] == 0 and proof['provider_call_id'],
            'Invocation was dispatched or did not await approval')
    calls = rows(db, 'SELECT id,tool_name FROM tool_invocations WHERE run_id=?', (run,))
    require(calls == [{'id': invocation, 'tool_name': 'write_file'}],
            'Unexpected tool calls before approval')
    return proof


def verify_history(messages, run, kind, content=None, identity=None):
    selected = [message for message in messages if message['run_id'] == run
                and message['kind'] == kind]
    if content is not None:
        selected = [message for message in selected if message['content'] == content]
    if identity is not None:
        selected = [message for message in selected if message['id'] == identity]
    require(selected and all(message['status'] == 'recorded' and message['turn_id']
                             for message in selected), 'Missing durable ' + kind)
    return selected


def exercise(args, app, evidence):
    require(json.loads((RUNTIME / 'config.json').read_text()) == CONFIG,
            'Prepared config changed; verify effective serve configuration separately')
    workspace = RUNTIME / 'workspace'
    require(workspace.is_dir() and not workspace.is_symlink()
            and not (workspace / FILENAME).exists()
            and not (workspace / FILENAME).is_symlink(),
            'Workspace is missing or acceptance target already exists')
    dbpath = RUNTIME / 'harness.db'
    require(dbpath.is_file(), 'Dedicated application database is missing')
    db = sqlite3.connect(dbpath.as_uri() + '?mode=ro', uri=True)
    db.row_factory = sqlite3.Row
    try:
        require(app.request('GET', '/healthz').get('provider') == 'openai',
                'Application is not using the OpenAI-compatible provider')
        require(app.request('GET', '/v1/sessions') == [], 'Application database is not fresh')
        session = app.request('POST', '/v1/sessions', expected=201)['id']
        evidence['session_id'] = session
        app.save()
        chat = app.request('POST', f'/v1/sessions/{session}/runs?wait=1',
                           {'message': 'Reply with one short greeting. Do not use external tools.'}, 201)
        evidence['chat'] = chat
        require(chat['status'] == 'COMPLETED' and chat.get('final_answer'),
                'Real application reply did not complete')
        history = app.messages(session)
        evidence['chat_messages'] = history
        verify_history(history, chat['id'], 'response', chat['final_answer'])
        evidence['chat_sources'] = responses(db, session)
        require(any(row['run_id'] == chat['id'] for row in evidence['chat_sources']),
                'Chat run is not linked to this database')
        prompt = (f'First say you are requesting approval. Then call write_file exactly once '
                  f'with path {FILENAME!r} and content {MARKER!r}. Do not call any other '
                  'tools or delegate. Once the write is approved or denied, finish with a '
                  'short explanation; never retry or use another write mechanism.')
        waiting = app.request('POST', f'/v1/sessions/{session}/runs?wait=1',
                              {'message': prompt}, 201)
        evidence['waiting'] = waiting
        require(waiting['status'] == 'WAITING' and waiting.get('wait_reason') == 'APPROVAL',
                'Model did not naturally trigger WAITING(APPROVAL)')
        pending = waiting.get('pending_invocation', {})
        require(pending.get('tool') == 'write_file'
                and pending.get('arguments') == {'path': FILENAME, 'content': MARKER},
                'Pending operation is outside the exact harmless write')
        require(not (workspace / FILENAME).exists(), 'Write executed before approval')
        queue = app.request('GET', '/v1/approvals')
        require(any(item['run_id'] == waiting['id'] and item['session_id'] == session
                    and item['wait_reason'] == 'approval' for item in queue),
                'Pending approval is absent from application queue')
        evidence['pending_queue'] = queue
        proof = verify_pending(db, waiting['id'], pending['invocation_id'])
        evidence['approval_source'] = proof
        history = app.messages(session)
        evidence['waiting_messages'] = history
        verify_history(history, waiting['id'], 'approval_requested',
                       identity='msg_approval_' + proof['approval_id'] + '_requested')
        app.save()
        terminal = app.request('POST', f'/v1/runs/{waiting["id"]}/responses?wait=1',
                               {'decision': args.decision})
        evidence['terminal'] = terminal
        require(terminal['id'] == waiting['id'] and terminal['status'] == 'COMPLETED',
                'Approved/denied run did not reach COMPLETED')
        approval = rows(db, 'SELECT id,state FROM approvals WHERE id=?',
                        (proof['approval_id'],))
        require(approval == [{'id': proof['approval_id'],
                             'state': {'approve': 'approved', 'deny': 'denied'}[args.decision]}],
                'Approval decision was not persisted')
        evidence['persisted_decision'] = approval
        calls = rows(db, 'SELECT id,tool_name FROM tool_invocations WHERE run_id=?',
                     (waiting['id'],))
        require(sum(call['tool_name'] == 'write_file' for call in calls) == 1
                and all(call['tool_name'] in ('write_file', 'finish_turn') for call in calls),
                'Unexpected tool calls after approval decision')
        require(not any(item['run_id'] == waiting['id']
                        for item in app.request('GET', '/v1/approvals')),
                'Terminal run still has a pending approval')
        target = workspace / FILENAME
        if args.decision == 'approve':
            require(not target.is_symlink() and target.is_file()
                    and target.read_text() == MARKER, 'Approved file result differs')
        else:
            require(not target.exists() and not target.is_symlink(), 'Denied write executed')
        final = app.messages(session)
        evidence['final_messages'] = final
        verify_history(final, waiting['id'], 'approval_decision', args.decision,
                       'msg_approval_' + proof['approval_id'] + '_decision')
        verify_history(final, waiting['id'], 'run_status', 'COMPLETED')
        before = {message['id']: message for message in history}
        after = {message['id']: message for message in final}
        require(all(after.get(key) == value for key, value in before.items()),
                'Earlier persisted transcript changed after resume')
        require(app.messages(session) == final, 'Repeated history read changed durable messages')
        evidence['response_sources'] = responses(db, session)
        evidence['status'] = 'application_checks_passed'
    finally:
        db.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--prepare', action='store_true', help='create only key-free runtime files')
    mode.add_argument('--run', action='store_true', help='real requests through an operator-managed service')
    parser.add_argument('--decision', choices=['approve', 'deny'], default='deny')
    parser.add_argument('--timeout', type=int, default=900, help='seconds per HTTP call; no retries')
    args = parser.parse_args()
    require(platform.node() == 'osaka', 'Acceptance is restricted to Osaka')
    require(1 <= args.timeout <= 1800, 'HTTP timeout must be between 1 and 1800 seconds')
    plan = {'status': 'blocked_not_executed', 'at_shanghai': now(), 'host': platform.node(),
            'blocker': BLOCKER, 'runtime': str(RUNTIME), 'application_origin': ORIGIN,
            'config': CONFIG, 'external_free_status': 'unverified_account_entitlement'}
    if args.prepare:
        require(not RUNTIME.is_symlink(), 'Runtime must not be a symlink')
        RUNTIME.mkdir(parents=True, exist_ok=True)
        require(not (RUNTIME / 'config.json').exists(), 'Refusing to replace runtime config')
        (RUNTIME / 'workspace').mkdir(exist_ok=False)
        write_json(RUNTIME / 'config.json', CONFIG, mode='x')
        plan['status'] = 'prepared_live_not_executed'
    elif args.run:
        evidence = {'status': 'running', 'started_at_shanghai': now(), 'host': platform.node(),
                    'application_origin': ORIGIN, 'requested_upstream': UPSTREAM,
                    'model': MODEL, 'decision': args.decision, 'http': [],
                    'external_free_status': 'unverified_account_entitlement',
                    'endpoint_evidence': 'Prepared config; verify the serve process environment separately',
                    'script_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
        write_json(RUNTIME / 'evidence.json', evidence, mode='x')
        app = Application(args.timeout, evidence)
        try:
            exercise(args, app, evidence)
        except Exception as error:
            evidence['status'] = 'failed'
            evidence['failure'] = str(error) if isinstance(error, AcceptanceError) else type(error).__name__
            session = evidence.get('session_id')
            if session:
                try:
                    retained = app.request('GET', f'/v1/sessions/{session}/runs')
                    evidence['retained_runs'] = retained
                    for run in retained:
                        if run['status'] in ('RUNNING', 'WAITING'):
                            app.request('POST', f'/v1/runs/{run["id"]}/cancel')
                    evidence['failure_messages'] = app.messages(session)
                except Exception as cleanup_error:
                    evidence['cleanup_error_type'] = type(cleanup_error).__name__
            evidence['finished_at_shanghai'] = now()
            app.save()
            print(json.dumps({'status': 'failed', 'evidence': str(RUNTIME / 'evidence.json')}))
            return 1
        evidence['finished_at_shanghai'] = now()
        app.save()
        print(json.dumps({'status': evidence['status'], 'evidence': str(RUNTIME / 'evidence.json'),
                          'external_free_status': evidence['external_free_status']}))
        return 0
    print(json.dumps(plan, ensure_ascii=False, indent=2))
    return 0 if args.prepare else 2


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (AcceptanceError, OSError) as error:
        print(json.dumps({'status': 'blocked', 'reason': str(error) if isinstance(error, AcceptanceError)
                          else type(error).__name__}))
        sys.exit(2)
