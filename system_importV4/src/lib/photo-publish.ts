/** Executed on the uploads host through sudo python3. It never overwrites a path.
 * The archive is verified in a private staging directory before any file is
 * published. os.link makes a complete file visible atomically, without clobbering
 * an existing original (including when two imports race).
 */
export const PHOTO_PUBLISH_SCRIPT = String.raw`
import hashlib, json, os, pathlib, stat, sys, tarfile, tempfile

root = pathlib.Path(sys.argv[1]).resolve(strict=True)
archive = tarfile.open(fileobj=sys.stdin.buffer, mode='r|gz')
members = iter(archive)
header = next(members)
if header.name != '.photo-import-manifest.json' or not header.isfile() or header.size > 16 * 1024 * 1024:
    raise ValueError('archive must start with a bounded file manifest')
expected = json.load(archive.extractfile(header))
if not isinstance(expected, dict) or not expected:
    raise ValueError('empty file manifest')

def parts(name):
    p = pathlib.PurePosixPath(name)
    if p.is_absolute() or '..' in p.parts or chr(92) in name or ':' in name:
        raise ValueError('unsafe archive path')
    return p.parts

for name, info in expected.items():
    if len(parts(name)) != 3 or parts(name)[1] not in ('opd-photo', 'opd-photo-thumb'):
        raise ValueError('unexpected destination')
    if info['size'] <= 0 or len(info['sha256']) != 64:
        raise ValueError('invalid file digest')

def destination(name, create=False):
    current = root
    for part in parts(name)[:-1]:
        current = current / part
        if create:
            try: current.mkdir(mode=0o755)
            except FileExistsError: pass
        if current.exists() or current.is_symlink():
            mode = current.lstat().st_mode
            if not stat.S_ISDIR(mode) or stat.S_ISLNK(mode):
                raise ValueError('destination parent is not a real directory')
    return root.joinpath(*parts(name))

def check_existing(dest, info):
    try: mode = dest.lstat().st_mode
    except FileNotFoundError: return False
    if not stat.S_ISREG(mode):
        raise ValueError('existing destination is not a regular file')
    fd = os.open(dest, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
    with os.fdopen(fd, 'rb') as f:
        digest = hashlib.sha256()
        while True:
            block = f.read(1024 * 1024)
            if not block: break
            digest.update(block)
        if os.fstat(f.fileno()).st_size != info['size'] or digest.hexdigest() != info['sha256']:
            raise ValueError('existing photo has different bytes; refusing to overwrite: ' + str(dest))
    return True

with tempfile.TemporaryDirectory(prefix='.photo-import-', dir=root) as tmp:
    staged = pathlib.Path(tmp)
    seen = set()
    with archive:
        for member in members:
            name = '/'.join(parts(member.name))
            if member.isdir():
                if name and not any(k.startswith(name + '/') for k in expected):
                    raise ValueError('unexpected archive directory')
                continue
            if name not in expected or name in seen or not member.isfile():
                raise ValueError('unexpected, duplicate or linked archive member')
            info = expected[name]
            if member.size != info['size']:
                raise ValueError('archive size mismatch')
            dest = staged.joinpath(*parts(name))
            dest.parent.mkdir(parents=True, exist_ok=True)
            digest = hashlib.sha256()
            with archive.extractfile(member) as src, dest.open('xb') as out:
                while True:
                    block = src.read(1024 * 1024)
                    if not block: break
                    digest.update(block)
                    out.write(block)
            if digest.hexdigest() != info['sha256']:
                raise ValueError('archive digest mismatch')
            dest.chmod(0o644)
            seen.add(name)
    if seen != set(expected):
        raise ValueError('archive is missing expected files')
    # Check every existing original before publishing any new file.
    for name, info in expected.items():
        check_existing(destination(name), info)
    created, existing = 0, 0
    for name, info in expected.items():
        dest = destination(name, create=True)
        try:
            os.link(staged.joinpath(*parts(name)), dest)
            created += 1
        except FileExistsError:
            check_existing(dest, info)
            existing += 1
    print(json.dumps({'created': created, 'existing': existing}))
`;
