#!/usr/bin/env python3
"""Static, non-executing extraction of upstream SQLAlchemy declarations.
SPDX-License-Identifier: AGPL-3.0-only
Run with an explicitly pinned orm_models.py path. Never imports upstream code.
"""
import ast,json,sys
s=ast.parse(open(sys.argv[1],encoding='utf-8').read())
tables=[]
for cls in s.body:
 if not isinstance(cls,ast.ClassDef):continue
 name=next((ast.literal_eval(x.value) for x in cls.body if isinstance(x,ast.Assign) and any(isinstance(t,ast.Name) and t.id=='__tablename__' for t in x.targets)),None)
 if not name:continue
 t=dict(name=name,columns=[],primary_key=[],unique=[],indexes=[])
 for x in cls.body:
  if isinstance(x,ast.AnnAssign) and isinstance(x.value,ast.Call) and getattr(x.value.func,'id',None)=='mapped_column':
   call=x.value;args=list(call.args);kwargs={k.arg:k.value for k in call.keywords};cn=x.target.id
   if args and isinstance(args[0],ast.Constant) and isinstance(args[0].value,str):cn=args.pop(0).value
   ann=ast.unparse(x.annotation);c=dict(name=cn,kind='integer' if 'int' in ann else 'string',nullable='Optional' in ann)
   if 'nullable' in kwargs:c['nullable']=ast.literal_eval(kwargs['nullable'])
   for arg in args:
    expr=ast.unparse(arg)
    if expr=='BigInteger':c['kind']='bigint'
    elif expr=='Integer':c['kind']='integer'
    elif expr=='Boolean':c['kind']='bool'
    elif expr=='NaiveDateTime':c['kind']='datetime'
    elif expr.startswith('String('):c['kind']='string';c['length']=ast.literal_eval(arg.args[0])
    elif expr.startswith('TEXT'):c['kind']='text';c['medium']=('MEDIUMTEXT' in expr)
    elif expr.startswith('DECIMAL('):c['kind']='decimal';c['precision']=ast.literal_eval(arg.args[0]);c['scale']=ast.literal_eval(arg.args[1])
    elif expr.startswith('Enum('):c['kind']='string';c['enum']=[ast.literal_eval(a) for a in arg.args];c['length']=max(map(len,c['enum']))
    elif expr.startswith('ForeignKey('):c['reference']=ast.literal_eval(arg.args[0]);c['on_delete']=next((ast.literal_eval(k.value) for k in arg.keywords if k.arg=='ondelete'),'NO ACTION')
   if kwargs.get('primary_key') and ast.literal_eval(kwargs['primary_key']):t['primary_key'].append(cn);c['nullable']=False
   if kwargs.get('autoincrement'):c['auto']=ast.literal_eval(kwargs['autoincrement'])
   if 'default' in kwargs:
    if isinstance(kwargs['default'],ast.Name):c['now_default']=kwargs['default'].id=='get_now'
    else:c['has_default']=True;c['default']=ast.literal_eval(kwargs['default'])
   if 'onupdate' in kwargs and isinstance(kwargs['onupdate'],ast.Name):c['now_update']=kwargs['onupdate'].id=='get_now'
   if 'server_default' in kwargs:c['server_default']=ast.literal_eval(kwargs['server_default'])
   if kwargs.get('unique') and ast.literal_eval(kwargs['unique']):t['unique'].append([cn])
   if kwargs.get('index') and ast.literal_eval(kwargs['index']):t['indexes'].append(dict(name='ix_'+name+'_'+cn,columns=[cn]))
   t['columns'].append(c)
  elif isinstance(x,ast.Assign) and any(isinstance(v,ast.Name) and v.id=='__table_args__' for v in x.targets):
   for call in x.value.elts:
    if not isinstance(call,ast.Call):continue
    cols=[ast.literal_eval(a) for a in call.args]
    if call.func.id=='UniqueConstraint':t['unique'].append(cols)
    elif call.func.id=='Index':
     idx=dict(name=cols[0],columns=cols[1:])
     for k in call.keywords:
      if k.arg=='mysql_length':idx['mysql_length']=ast.literal_eval(k.value)
     t['indexes'].append(idx)
 tables.append(t)
byname={t['name']:t for t in tables}
for t in tables:
 for c in t['columns']:
  if 'reference' in c:
   tab,col=c['reference'].split('.')
   ref=next(c for c in byname[tab]['columns'] if c['name']==col)
   c['kind']=ref['kind']
   if 'length' in ref:c['length']=ref['length']
print(json.dumps(tables,ensure_ascii=False,indent=2))
