#!/usr/bin/env python3
"""Run launcher.sh (the card's run) on the host with a fake app: after a failure it restarts, waiting
5 s and doubling; an exit of 0 ends the loop and tidies /tmp; a second run while it runs starts
nothing; a TERM to the loop's pid stops the app and tidies,
and an app that ignores it is killed 2 s later.
Usage: test_launcher.py"""
import os
import pathlib
import re
import signal
import subprocess
import tempfile
import time

source = pathlib.Path(__file__).with_name('launcher.sh').read_text()
app = re.search(r'^pid=/tmp/q2-([a-z]+)$', source, re.M)[1]


def wait(ok, what, secs=10):
    end = time.time() + secs
    while not ok():
        if time.time() > end:
            raise SystemExit(f'test_launcher.py: timed out waiting for {what}')
        time.sleep(0.02)


def rig(tmp, body):
    """The card's folder with the fake app and the launcher, /tmp's stand-in, and an environment
    whose sleep only records its argument, but for the stop's 2 s grace, which it sleeps."""
    card, t, bin_ = tmp/'card'/f'.{app}', tmp/'t', tmp/'bin'
    for d in (card, t, bin_):
        d.mkdir(parents=True)
    exe = card/f'q2-{app}'
    exe.write_text('#!/bin/sh\n' + body)
    exe.chmod(0o755)
    (card/'run').write_text(source.replace(f'/tmp/q2-{app}', f'{t}/q2-{app}').replace('cd /tmp\n', f'cd {t}\n'))
    (bin_/'sleep').write_text('#!/bin/sh\n[ "$1" = 2 ] && exec /bin/sleep 2\necho "$1" >> "$Q2_TEST/sleeps"\n')
    (bin_/'sleep').chmod(0o755)
    return card, t, dict(os.environ, Q2_TEST=str(tmp), Q2_T=str(t), PATH=f'{bin_}:/usr/bin:/bin')


def lines(path):
    return path.read_text().splitlines() if path.exists() else []


import atexit
atexit.register(lambda: subprocess.run(['pkill', '-KILL', '-f', f'{tempfile.gettempdir()}/q2-launcher-'], stderr=subprocess.DEVNULL))
with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo "$@" >> "$Q2_TEST/args"\n'
                            'cat "$Q2_T/q2-%s.crash" >> "$Q2_TEST/crashes" 2>/dev/null\n'
                            '[ "$(wc -l < "$Q2_TEST/args")" -ge 3 ] && exit 0\necho "panic: boom $(wc -l < "$Q2_TEST/args")" >&2\nexit 2\n' % app)
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: len(lines(tmp/'args')) == 3 and not (t/f'q2-{app}').exists(), 'the loop to end')
    assert lines(tmp/'args') == [f'-dir {card}'] * 3, lines(tmp/'args')
    assert lines(tmp/'sleeps') == ['5', '10'], lines(tmp/'sleeps')
    # each restart finds what the failed run left on stderr and its exit code, for log.txt (M43)
    assert lines(tmp/'crashes') == ['panic: boom 1', 'exit 2', 'panic: boom 2', 'exit 2'], lines(tmp/'crashes')
    assert not list(t.iterdir()), list(t.iterdir())  # the pid, the copied app and launcher gone

with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo start >> "$Q2_TEST/starts"\n'
                            'trap \'echo term >> "$Q2_TEST/terms"; exit 0\' TERM\n'
                            'while :; do /bin/sleep 0.05; done\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: lines(tmp/'starts') == ['start'], 'the app')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)  # runs already
    time.sleep(0.3)
    assert lines(tmp/'starts') == ['start'], lines(tmp/'starts')
    os.kill(int((t/f'q2-{app}').read_text()), signal.SIGTERM)
    wait(lambda: not list(t.iterdir()), 'the TERM to tidy')
    assert lines(tmp/'terms') == ['term'] and not lines(tmp/'sleeps'), (lines(tmp/'terms'), lines(tmp/'sleeps'))
with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:  # stopped in a crash loop: the trace stays
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo start >> "$Q2_TEST/starts"\n'
                            '[ "$(wc -l < "$Q2_TEST/starts")" -lt 2 ] && { echo "panic: early" >&2; exit 2; }\n'
                            'trap \'exit 0\' TERM\n'
                            'while :; do /bin/sleep 0.05; done\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: len(lines(tmp/'starts')) == 2, 'the restart')
    os.kill(int((t/f'q2-{app}').read_text()), signal.SIGTERM)
    wait(lambda: not (t/f'q2-{app}').exists(), 'the TERM to tidy')
    assert [f.name for f in t.iterdir()] == [f'q2-{app}.crash'], list(t.iterdir())
    assert lines(t/f'q2-{app}.crash') == ['panic: early', 'exit 2'], lines(t/f'q2-{app}.crash')
with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:  # an app that ignores TERM is killed
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo start >> "$Q2_TEST/starts"\n'
                            'trap "" TERM\n'
                            'while :; do /bin/sleep 0.05; done\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: lines(tmp/'starts') == ['start'], 'the app')
    loop = int((t/f'q2-{app}').read_text())
    os.kill(loop, signal.SIGTERM)
    wait(lambda: not list(t.iterdir()), 'the KILL to tidy', secs=3)
    assert subprocess.run(['pgrep', '-f', str(t/f'q2-{app}.bin')]).returncode == 1, 'the app still runs'
with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:  # a TERM in the backoff ends it at once
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo start >> "$Q2_TEST/starts"\nexit 2\n')
    (tmp/'bin/sleep').write_text('#!/bin/sh\nexec /bin/sleep 30\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: lines(tmp/'starts') == ['start'] and (t/f'q2-{app}.crash').exists(), 'the backoff')
    os.kill(int((t/f'q2-{app}').read_text()), signal.SIGTERM)
    wait(lambda: not (t/f'q2-{app}').exists(), 'the TERM in the backoff', secs=1)
    subprocess.run(['pkill', '-f', f'{tmp}/bin/sleep'])
print(f'Launcher (q2-{app}): restarts with backoff, keeps a crash for the next start, exit 0, one at a time, TERM passed and KILL after 2 s.')
