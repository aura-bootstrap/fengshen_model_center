"""Native-only contract tests. No fake decoder and no automatic model download."""
import argparse
import base64
import json
import subprocess
import threading
import time
import wave

P = 'fengshen.whispercpp.stream/v1'
OPTIONS = dict(window_sec=5, step_sec=.5, stable_rounds=2,
               timestamp_tolerance_sec=.2, finalize_delay_sec=2)

def msg(t, **kw):
    return dict(protocol=P, type=t, request_id='native-test', episode_number=1, **kw)

def run(exe, args, data):
    p = subprocess.run([exe, *args], input=data, capture_output=True, timeout=30)
    lines = [json.loads(x) for x in p.stdout.splitlines()]
    return p, lines

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('exe')
    ap.add_argument('--model')
    ap.add_argument('--wav')
    ap.add_argument('--vad-model', help='Explicit local VAD model; never downloaded')
    ap.add_argument('--window', type=float, default=30)
    a = ap.parse_args()
    OPTIONS['window_sec'] = a.window
    p, lines = run(a.exe, ['--metadata'], b'')
    assert p.returncode == 0 and lines[0]['protocol'] == P
    assert lines[0]['native'] and lines[0]['vad_supported']
    for flags in [['--vad'], ['--vad-model', 'vad.bin'],
                  ['--vad', '--vad', '--vad-model', 'vad.bin'],
                  ['--vad', '--vad-model', 'a', '--vad-model', 'b']]:
        q, errors = run(a.exe, ['--fengshen-stream-v1', '--model', 'absent.bin', *flags], b'')
        assert q.returncode != 0 and errors[-1]['error_code'] == 'arguments'
    start = msg('start', duration_sec=1, format='s16le/16000/1', options=OPTIONS)
    invalid = [b'\n', b'{}\n', b'{bad}\n', b'x' * (1048576 + 1) + b'\n',
               (json.dumps(dict(start, unknown=True))+'\n').encode(),
               (json.dumps(dict(start, options=dict(OPTIONS, window_sec=121)))+'\n').encode(),
               (json.dumps(msg('audio', revision=1))+'\n').encode(),
               b'{"protocol":"fengshen.whispercpp.stream/v1","type":"start","type":"audio","request_id":"native-test"}\n']
    for data in invalid:
        p, lines = run(a.exe, ['--fengshen-stream-v1', '--model', 'absent.bin'], data)
        assert p.returncode != 0 and lines and lines[-1]['type'] == 'error', (data[:100], p, lines)
        assert not any(x['type'] in ('ready', 'final') for x in lines)
    p, lines = run(a.exe, ['--fengshen-stream-v1', '--model', 'absent.bin'], b'')
    assert p.returncode != 0 and not any(x['type'] == 'final' for x in lines)
    p, _ = run(a.exe, ['--fengshen-stream-v1', '--shell', 'anything'], b'')
    assert p.returncode != 0
    print('PASS metadata + malformed/unknown/oversized/options/order/duplicate/EOF/argv rejection')
    if not a.model:
        return
    assert a.wav, '--wav required with --model'
    with wave.open(a.wav, 'rb') as w:
        assert (w.getnchannels(), w.getsampwidth(), w.getframerate()) == (1, 2, 16000)
        pcm = w.readframes(w.getnframes())
    duration = len(pcm)/32000
    cmd = [a.exe, '--fengshen-stream-v1', '--model', a.model, '--threads', '4']
    if a.vad_model:
        cmd.extend(['--vad', '--vad-model', a.vad_model])
        # Real VAD must produce an empty hypothesis for silent raw audio.
        silent = bytes(32000)
        frames = [dict(start, duration_sec=1),
                  msg('audio', revision=1, start_sec=0, end_sec=1, sample_count=16000,
                      pcm=base64.b64encode(silent).decode()),
                  msg('finalize', revision=2, committed_through_sec=1)]
        q, lines = run(a.exe, cmd[1:], ('\n'.join(map(json.dumps, frames))+'\n').encode())
        assert q.returncode == 0 and [x['type'] for x in lines] == ['ready', 'hypothesis', 'final'], lines
        assert lines[1]['items'] == [] and lines[2]['items'] == [], lines
    # Model-backed negative cases must pass admission before malformed audio.
    cases = [msg('audio', revision=2, start_sec=0, end_sec=.5, sample_count=8000, pcm='A'*21336),
             msg('audio', revision=1, start_sec=.1, end_sec=.5, sample_count=8000, pcm='A'*21336),
             msg('audio', revision=1, start_sec=0, end_sec=.5, sample_count=8000, pcm='!'),
             msg('audio', revision=1, start_sec=0, end_sec=.5, sample_count=320001, pcm=''),
             msg('finalize', revision=1, committed_through_sec=duration)]
    real_start = dict(start, duration_sec=duration)
    for bad in cases:
        data = ('\n'.join(map(json.dumps, [real_start, bad]))+'\n').encode()
        q, lines = run(a.exe, cmd[1:], data)
        assert q.returncode != 0 and lines[0]['type'] == 'ready' and lines[-1]['type'] == 'error', lines
        assert not any(x['type'] == 'final' for x in lines)
    # Idle EOF and in-flight EOF are cancellation, never a successful final.
    q = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    q.stdin.write((json.dumps(real_start)+'\n').encode()); q.stdin.flush()
    assert json.loads(q.stdout.readline())['type'] == 'ready'
    b = pcm[:16000]
    audio = msg('audio', revision=1, start_sec=0, end_sec=len(b)/32000,
                sample_count=len(b)//2, pcm=base64.b64encode(b).decode())
    q.stdin.write((json.dumps(audio)+'\n').encode()); q.stdin.flush()
    time.sleep(.05)  # Deliberately interrupt a real ggml decode, not a retry loop.
    q.stdin.close()
    assert q.wait(timeout=15) != 0
    lines = [json.loads(x) for x in q.stdout.read().splitlines()]
    assert not any(x['type'] == 'final' for x in lines)
    # Consumer stalls stdout: native cannot read an unbounded input queue.
    q = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    q.stdin.write((json.dumps(real_start)+'\n').encode()); q.stdin.flush()
    assert json.loads(q.stdout.readline())['type'] == 'ready'
    finished = threading.Event()
    def flood():
        try:
            for revision in range(1, 4001):
                frame = msg('audio', revision=revision, start_sec=(revision-1)/16000,
                            end_sec=revision/16000, sample_count=1, pcm='AAA=')
                q.stdin.write((json.dumps(frame)+'\n').encode()); q.stdin.flush()
        except (BrokenPipeError, OSError, ValueError):
            pass
        finally:
            finished.set()
    writer = threading.Thread(target=flood, daemon=True)
    writer.start()
    try:
        assert not finished.wait(1), 'writer drained despite stalled stdout: missing backpressure'
    finally:
        q.kill(); q.wait(timeout=10); writer.join(timeout=10)
        assert not writer.is_alive()
        try:
            q.stdin.close()
        except OSError:
            pass
        q.stdout.close()
    print('PASS model-backed revision/boundary/PCM/count/early-final/EOF cancellation/backpressure')
    p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    def exchange(m):
        p.stdin.write((json.dumps(m)+'\n').encode()); p.stdin.flush()
        timer = threading.Timer(90, p.kill)
        timer.start()
        try:
            line = p.stdout.readline(1048577)
        finally:
            timer.cancel()
        assert line and len(line) <= 1048576
        r = json.loads(line)
        assert r['protocol'] == P and r['request_id'] == 'native-test', r
        assert r['type'] != 'error', r
        return r
    try:
        assert exchange(dict(start, duration_sec=duration))['type'] == 'ready'
        previous = []
        committed = []
        rounds = []
        evidence = []
        for rev, offset in enumerate(range(0, len(pcm), 16000), 1):
            b = pcm[offset:offset+16000]
            end = (offset+len(b))/32000
            r = exchange(msg('audio', revision=rev, start_sec=offset/32000, end_sec=end,
                             sample_count=len(b)//2, pcm=base64.b64encode(b).decode()))
            assert r['type'] == 'hypothesis' and r['revision'] == rev and r['committed_through_sec'] == end
            items = r.get('items', [])
            assert items[:len(committed)] == committed, 'committed prefix drift'
            tail = items[len(committed):]
            nr = []
            for i, item in enumerate(tail):
                nr.append(rounds[i]+1 if i < len(previous) and item == previous[i] else 1)
            count = 0
            for item, n in zip(tail, nr):
                if n < 2 or item['end_sec']+2 > end+1e-9: break
                count += 1
            committed.extend(tail[:count]); previous = tail[count:]; rounds = nr[count:]
            for item in items:
                assert 0 <= item['start_sec'] < item['end_sec'] <= end
                assert item['words'] and ''.join(x['text'] for x in item['words']) == item['text']
                last = item['start_sec']
                for word in item['words']:
                    assert last <= word['start_sec'] < word['end_sec'] <= item['end_sec']
                    last = word['end_sec']
            evidence.append(r)
        r = exchange(msg('finalize', revision=rev+1, committed_through_sec=duration))
        assert r['type'] == 'final' and r['final'] and r['items'][:len(committed)] == committed
        assert r['items'] and any(x['text'].strip() for x in r['items']), 'real speech required'
        p.stdin.close(); assert p.wait(timeout=20) == 0
        print(json.dumps(dict(real_inference=True, revisions=len(evidence), final=r), ensure_ascii=False))
    finally:
        if p.poll() is None: p.kill(); p.wait()

if __name__ == '__main__':
    main()
