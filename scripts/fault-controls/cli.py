"""Source-bound CLI builds shared by unraced packages and focused phases."""
from pathlib import Path
import re
import subprocess
from checks import require

PREFIX='github.com/zalkilabs/zalki'


def identity(info,head,paths,modified=False):
    require(type(info) is dict and info.get('Path')==PREFIX+'/cmd/zalki' and
            info.get('GoVersion')==paths['GOVERSION'],'fresh CLI identity differs')
    require(type(modified) is bool and type(info.get('Settings')) is list,'missing CLI build settings')
    settings={}
    for item in info['Settings']:
        require(type(item) is dict and set(item)<= {'Key','Value'} and type(item.get('Key')) is str and
                bool(item['Key']) and type(item.get('Value','')) is str,'invalid CLI build setting')
        require(item['Key'] not in settings,'duplicate CLI build setting')
        settings[item['Key']]=item.get('Value','')
    require(settings.get('-race','false')=='false','CLI was built with race instrumentation')
    require(settings.get('vcs.revision')==head and settings.get('vcs.modified')==str(modified).lower(),
            'CLI VCS identity differs from the exact source state')
    for key in ['GOOS','GOARCH','CGO_ENABLED']:require(settings.get(key)==paths[key],'CLI native build identity differs')
    return settings


def clone_source(repo,head,tree,source):
    subprocess.run(['git','-C',str(repo),'clone','--no-hardlinks','--no-checkout','--',str(repo),str(tree)],check=True,timeout=120)
    subprocess.run(['git','-C',str(tree),'checkout','--detach',head],check=True,timeout=120)
    require((tree/'.git').is_dir() and not (tree/'.git').is_symlink(),'fresh CLI source requires a real Git directory')
    require(source.git(tree,'rev-parse','HEAD')==head and not source.git(tree,'status','--porcelain','--untracked-files=all'),
            'fresh CLI source is not the clean actual head')


def build(tree,head,paths,folder,source,command,modified=False,timeout=240):
    folder.mkdir(mode=0o700)
    before=source.source_fingerprint(tree)
    require((tree/'.git').is_dir() and not (tree/'.git').is_symlink(),'CLI build requires a real Git directory')
    require(source.git(tree,'rev-parse','HEAD')==head and
            bool(source.git(tree,'status','--porcelain','--untracked-files=all'))==modified,'CLI source state differs')
    env=source.environment(paths,folder/'home',folder/'unused-cache',folder/'unused-linter',folder/'unused.json')
    temporary=folder/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    binary=folder/'zalki';go=str(Path(paths['GOROOT'])/'bin/go')
    built=command([go,'build','-race=false','-buildvcs=true','-p=1','-mod=readonly','-o',str(binary),'./cmd/zalki'],
                  tree,env,folder/'build.log',0,timeout=timeout)
    metadata=command([go,'version','-m','-json',str(binary)],tree,env,folder/'buildinfo.json',0)
    info=source.read_json(folder/'buildinfo.json');settings=identity(info,head,paths,modified)
    require(source.source_fingerprint(tree)==before,'CLI build changed source')
    record=dict(binary_sha256=source.digest(binary),source_fingerprint=before,head=head,modified=modified,
                build=built,metadata=metadata,build_info=info,settings=settings)
    return binary,record


def valid_fingerprint(value):
    return type(value) is dict and set(value)=={'files','sha256'} and type(value['files']) is int and value['files']>0 and \
        type(value['sha256']) is str and re.fullmatch('[0-9a-f]{64}',value['sha256']) is not None


def verify(record,head,paths,fingerprint,modified):
    require(valid_fingerprint(fingerprint),'missing complete CLI phase source fingerprint')
    require(type(record) is dict and record.get('head')==head and record.get('modified') is modified and
            record.get('source_fingerprint')==fingerprint,'CLI record is not bound to its phase source')
    require(type(record.get('binary_sha256')) is str and re.fullmatch('[0-9a-f]{64}',record['binary_sha256']),
            'missing actual CLI binary identity')
    require(identity(record.get('build_info'),head,paths,modified)==record.get('settings'),'CLI settings differ from raw metadata')
    for command in ['build','metadata']:
        receipt=record.get(command)
        require(type(receipt) is dict and receipt.get('exit')==0 and type(receipt.get('log_sha256')) is str and
                re.fullmatch('[0-9a-f]{64}',receipt['log_sha256']),'CLI command did not complete successfully')
    args=record['build'].get('argv')
    require(type(args) is list and len(args)>1 and all(type(a) is str for a in args) and
            args[1]=='build' and '-race=false' in args and '-buildvcs=true' in args and './cmd/zalki' in args,
            'CLI build command differs from the required profile')
