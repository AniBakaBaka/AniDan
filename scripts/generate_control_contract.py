#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Derive static request/schema contracts without importing upstream application.
Requires Python FastAPI/Pydantic. Only declarative model classes and stripped
endpoint signatures are loaded; original endpoint bodies never execute.
"""
import argparse, ast, copy, datetime, enum, json, pathlib, types, typing
import fastapi
import pydantic
from fastapi.openapi.utils import get_openapi
p=argparse.ArgumentParser();p.add_argument('--upstream',type=pathlib.Path,required=True);p.add_argument('--output',type=pathlib.Path,required=True);a=p.parse_args()
root=a.upstream
ns={k:getattr(typing,k) for k in ['Any','Dict','List','Optional','Union','Tuple','Callable','Set','Annotated']}
ns.update({'__name__':'anidan_reference_contract','Enum':enum.Enum,'datetime':datetime.datetime,'BaseModel':pydantic.BaseModel,'ConfigDict':pydantic.ConfigDict,'Field':pydantic.Field,'model_validator':pydantic.model_validator,'Query':fastapi.Query,'Path':fastapi.Path,'Body':fastapi.Body,'Request':fastapi.Request,'status':fastapi.status})
def classes(path,namespace):
    tree=ast.parse(path.read_text())
    for node in tree.body:
        if isinstance(node,ast.ClassDef):
            # Validator bodies are not part of JSON schema; strip custom methods
            # to avoid executing application behavior during model construction.
            node=copy.deepcopy(node)
            node.body=[n for n in node.body if not isinstance(n,(ast.FunctionDef,ast.AsyncFunctionDef))]
            if not node.body:node.body=[ast.Pass()]
            exec(compile(ast.fix_missing_locations(ast.Module(body=[node],type_ignores=[])),str(path),'exec'),namespace)
    return tree
base=dict(ns);classes(root/'src/db/models.py',base);ns['models']=types.SimpleNamespace(**{k:v for k,v in base.items() if isinstance(v,type)})
classes(root/'src/api/control/models.py',ns)
app=fastapi.FastAPI();operations=[]
for path in sorted((root/'src/api/control').glob('*_routes.py')):
    local=dict(ns);tree=classes(path,local)
    prefix=''
    for node in tree.body:
        if isinstance(node,ast.Assign) and isinstance(node.value,ast.Call) and isinstance(node.value.func,ast.Name) and node.value.func.id=='APIRouter':
            prefix=next((ast.literal_eval(k.value) for k in node.value.keywords if k.arg=='prefix'),'')
    for f in tree.body:
        if not isinstance(f,ast.AsyncFunctionDef):continue
        for d in f.decorator_list:
            if not isinstance(d,ast.Call) or not isinstance(d.func,ast.Attribute) or not isinstance(d.func.value,ast.Name) or d.func.value.id!='router':continue
            method=d.func.attr.upper()
            if method not in ['GET','POST','PUT','PATCH','DELETE']:continue
            route='/api/control'+prefix+ast.literal_eval(d.args[0]);fn=copy.deepcopy(f);fn.decorator_list=[];fn.returns=None
            doc=ast.get_docstring(f) or '';fn.body=[ast.Expr(ast.Constant(doc)),ast.Return(ast.Constant(None))]
            oldargs=fn.args.args;defaults=[None]*(len(oldargs)-len(fn.args.defaults))+fn.args.defaults
            args=[];defs=[]
            for arg,default in zip(oldargs,defaults):
                if isinstance(default,ast.Call) and isinstance(default.func,ast.Name) and default.func.id=='Depends':continue
                if isinstance(arg.annotation,ast.Name) and arg.annotation.id in ['Request','BackgroundTasks']:continue
                args.append(arg);defs.append(default)
            fn.args.args=args;fn.args.defaults=[v for v in defs if v is not None]
            exec(compile(ast.fix_missing_locations(ast.Module(body=[fn],type_ignores=[])),str(path),'exec'),local)
            opts={}
            for k in d.keywords:
                if k.arg in ['summary','description','status_code','response_model','response_model_exclude_none','response_model_by_alias']:
                    opts[k.arg]=eval(compile(ast.Expression(k.value),str(path),'eval'),local)
            app.add_api_route(route,local[f.name],methods=[method],tags=['External Control API'],**opts)
            operations.append({'name':f.name,'path':route,'method':method,'source':str(path.relative_to(root)),'line':f.lineno})
schema=get_openapi(title='AniDan External Control API',version='0.1.0-dev',openapi_version='3.0.3',routes=app.routes)
schema['x-upstream-commit']='01751526f6e4154bcc8f517481d02b68cb2684a9';schema['x-generation']={'method':'declarative AST stripped-signature derivation; not running upstream app','fastapi':fastapi.__version__,'pydantic':pydantic.__version__}
schema['components']['securitySchemes']={'APIKeyHeader':{'type':'apiKey','in':'header','name':'X-API-KEY'},'APIKeyQuery':{'type':'apiKey','in':'query','name':'api_key'}}
for path,values in schema['paths'].items():
 for method,op in values.items():
  op['security']=[{'APIKeyHeader':[]},{'APIKeyQuery':[]}]
  op['x-implementation-status']='contract reference; actual Go handler must exist; behavior requires tests'
  if not path.startswith('/api/control/'):raise RuntimeError(path)
a.output.parent.mkdir(parents=True,exist_ok=True);a.output.write_text(json.dumps(schema,ensure_ascii=False,indent=2)+'\n')
print(json.dumps({'operations':len(operations),'paths':len(schema['paths']),'output':str(a.output)}))
