// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const maxRemoteObjects = 4096

func (r *remoteTarget) mysqlMetadataVisible(ctx context.Context) error {
	// Information_schema filters results by effective privileges. Direct grants
	// covering every object in this database must prove visibility independently
	// of an empty inventory. Role-only and table-specific grants fail closed.
	rows, err := r.queryer().QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if err != nil {
		return errors.New("cannot establish MySQL metadata visibility")
	}
	defer rows.Close()
	global, scoped := map[string]bool{}, map[string]bool{}
	count := 0
	for rows.Next() {
		var grant string
		if err = rows.Scan(&grant); err != nil {
			return errors.New("cannot establish MySQL metadata visibility")
		}
		count++
		if count > maxRemoteObjects {
			return errors.New("MySQL grant inventory exceeds bound")
		}
		if strings.HasPrefix(grant, "REVOKE ") {
			return errors.New("MySQL partial privilege revocations are unsupported")
		}
		if !strings.HasPrefix(grant, "GRANT ") {
			continue
		}
		on := strings.Index(grant, " ON ")
		to := strings.Index(grant, " TO ")
		if on < 0 || to < on {
			continue
		}
		scope := grant[on+4 : to]
		var destination map[string]bool
		switch scope {
		case "*.*":
			destination = global
		case r.store.Quote(r.namespace) + ".*":
			destination = scoped
		default:
			continue
		}
		for _, p := range strings.Split(grant[len("GRANT "):on], ",") {
			destination[strings.ToUpper(strings.TrimSpace(p))] = true
		}
	}
	if rows.Err() != nil {
		return errors.New("cannot establish MySQL metadata visibility")
	}
	has := func(p string) bool {
		return global["ALL PRIVILEGES"] || scoped["ALL PRIVILEGES"] || global[p] || scoped[p]
	}
	routines := global["ALL PRIVILEGES"] || global["SELECT"] || has("EXECUTE") || has("CREATE ROUTINE") || has("ALTER ROUTINE")
	if !has("SELECT") || !has("TRIGGER") || !has("EVENT") || !routines {
		return errors.New("MySQL migration requires direct database-wide metadata visibility for tables, triggers, events and routines")
	}
	return nil
}

func (r *remoteTarget) inventory(ctx context.Context) ([]string, error) {
	if r.store.Dialect == "mysql" {
		if err := r.mysqlMetadataVisible(ctx); err != nil {
			return nil, err
		}
		// Global SELECT also exposes routine definitions (including mysql.proc on
		// MariaDB); EVENT/TRIGGER are independently required above.
		var unsupported int
		err := r.queryer().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema=DATABASE())+(SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema=DATABASE())+(SELECT COUNT(*) FROM information_schema.events WHERE event_schema=DATABASE())`).Scan(&unsupported)
		if err != nil {
			return nil, errors.New("cannot inspect all MySQL namespace objects")
		}
		if unsupported != 0 {
			return nil, errors.New("remote namespace contains unsupported routines, triggers or events")
		}
		rows, err := r.queryer().QueryContext(ctx, `SELECT table_name,table_type,COALESCE(engine,''),COALESCE(table_collation,''),COALESCE(create_options,'') FROM information_schema.tables WHERE table_schema=DATABASE() ORDER BY table_name LIMIT 4097`)
		if err != nil {
			return nil, errors.New("cannot inspect MySQL namespace tables")
		}
		defer rows.Close()
		tables := []string{}
		for rows.Next() {
			var name, kind, engine, collation, options string
			if rows.Scan(&name, &kind, &engine, &collation, &options) != nil {
				return nil, errors.New("cannot inspect MySQL namespace tables")
			}
			if len(tables) >= maxRemoteObjects || kind != "BASE TABLE" || engine != "InnoDB" || collation != "utf8mb4_unicode_ci" || options != "" {
				return nil, errors.New("remote namespace contains an unsupported table, view or native object")
			}
			if _, ok := store.Schema[name]; !ok {
				return nil, errors.New("remote namespace contains an unknown table")
			}
			tables = append(tables, name)
		}
		if rows.Err() != nil {
			return nil, errors.New("cannot inspect MySQL namespace tables")
		}
		return tables, nil
	}
	// PostgreSQL catalogs, unlike information_schema, do not omit objects merely
	// because this role lacks SELECT on them. Refuse any inaccessible catalog.
	var native int
	err := r.queryer().QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM pg_catalog.pg_proc WHERE pronamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_type t WHERE typnamespace=current_schema()::regnamespace AND NOT
   (t.typtype='c' AND EXISTS(SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid=t.typrelid AND c.relkind='r')) AND NOT
   (t.typelem<>0 AND EXISTS(SELECT 1 FROM pg_catalog.pg_type e JOIN pg_catalog.pg_class c ON c.oid=e.typrelid WHERE e.oid=t.typelem AND e.typtype='c' AND e.typarray=t.oid AND c.relkind='r')))+
 (SELECT COUNT(*) FROM pg_catalog.pg_collation WHERE collnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_conversion WHERE connamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_operator WHERE oprnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_opclass WHERE opcnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_opfamily WHERE opfnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_ts_config WHERE cfgnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_ts_dict WHERE dictnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_ts_parser WHERE prsnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_ts_template WHERE tmplnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_extension WHERE extnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_statistic_ext WHERE stxnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_event_trigger WHERE evtenabled<>'D')+
 (SELECT COUNT(*) FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid WHERE c.relnamespace=current_schema()::regnamespace AND (NOT t.tgisinternal OR t.tgenabled<>'O'))+
 (SELECT COUNT(*) FROM pg_catalog.pg_rewrite w JOIN pg_catalog.pg_class c ON c.oid=w.ev_class WHERE c.relnamespace=current_schema()::regnamespace)+
 (SELECT COUNT(*) FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid=p.polrelid WHERE c.relnamespace=current_schema()::regnamespace)`).Scan(&native)
	if err != nil {
		return nil, errors.New("cannot inspect all PostgreSQL namespace objects")
	}
	if native != 0 {
		return nil, errors.New("remote namespace contains unsupported native objects")
	}
	rows, err := r.queryer().QueryContext(ctx, `SELECT c.relname,c.relkind::text,c.relpersistence::text,c.relispartition,c.relrowsecurity,c.relforcerowsecurity,c.relreplident::text,c.reloptions IS NULL,c.reltablespace=0 FROM pg_catalog.pg_class c WHERE c.relnamespace=current_schema()::regnamespace ORDER BY c.relname LIMIT 4097`)
	if err != nil {
		return nil, errors.New("cannot inspect PostgreSQL namespace relations")
	}
	type relation struct{ name, kind string }
	objects := []relation{}
	for rows.Next() {
		var name, kind, persistence, repl string
		var partition, rls, force, nooptions, defaultspace bool
		if rows.Scan(&name, &kind, &persistence, &partition, &rls, &force, &repl, &nooptions, &defaultspace) != nil {
			rows.Close()
			return nil, errors.New("cannot inspect PostgreSQL namespace relations")
		}
		if len(objects) >= maxRemoteObjects || persistence != "p" || partition || rls || force || !nooptions || !defaultspace || (kind != "r" && kind != "i" && kind != "S") || (kind == "r" && repl != "d") {
			rows.Close()
			return nil, errors.New("remote namespace contains an unsupported relation")
		}
		objects = append(objects, relation{name, kind})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errors.New("cannot inspect PostgreSQL namespace relations")
	}
	tables := []string{}
	present := map[string]bool{}
	for _, object := range objects {
		if object.kind == "r" {
			if _, ok := store.Schema[object.name]; !ok {
				return nil, errors.New("remote namespace contains an unknown table")
			}
			tables = append(tables, object.name)
			present[object.name] = true
		}
	}
	sequences := map[string]bool{}
	for _, object := range objects {
		if object.kind == "S" {
			sequences[object.name] = true
			if err = r.verifySequence(ctx, object.name, present); err != nil {
				return nil, err
			}
		} else if object.kind == "i" {
			var owner string
			if err = r.queryer().QueryRowContext(ctx, `SELECT t.relname FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class t ON t.oid=i.indrelid JOIN pg_catalog.pg_class x ON x.oid=i.indexrelid WHERE x.relnamespace=current_schema()::regnamespace AND x.relname=$1`, object.name).Scan(&owner); err != nil || !present[owner] {
				return nil, errors.New("remote namespace contains an unrelated index")
			}
		}
	}
	for _, name := range tables {
		for _, column := range store.Schema[name].Columns {
			if column.Auto && !sequences[name+"_"+column.Name+"_seq"] {
				return nil, errors.New("remote identity sequence is missing")
			}
		}
	}
	return tables, nil
}

func (r *remoteTarget) verifySequence(ctx context.Context, name string, present map[string]bool) error {
	var table, column, dependency string
	var start, min, max, increment, cache int64
	var cycle bool
	err := r.queryer().QueryRowContext(ctx, `SELECT t.relname,a.attname,d.deptype::text,s.seqstart,s.seqmin,s.seqmax,s.seqincrement,s.seqcache,s.seqcycle
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_sequence s ON s.seqrelid=c.oid
 JOIN pg_catalog.pg_depend d ON d.classid='pg_class'::regclass AND d.objid=c.oid AND d.refclassid='pg_class'::regclass AND d.deptype='i'
 JOIN pg_catalog.pg_class t ON t.oid=d.refobjid AND t.relnamespace=c.relnamespace
 JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=d.refobjsubid
 WHERE c.relnamespace=current_schema()::regnamespace AND c.relname=$1`, name).Scan(&table, &column, &dependency, &start, &min, &max, &increment, &cache, &cycle)
	if err != nil || !present[table] {
		return errors.New("remote namespace contains an unrelated sequence")
	}
	c, ok := store.Schema[table].Column(column)
	expectedMax := int64(9223372036854775807)
	if c.Kind == "integer" {
		expectedMax = 2147483647
	}
	if !ok || !c.Auto || name != table+"_"+column+"_seq" || start != 1 || min != 1 || max != expectedMax || increment != 1 || cache != 1 || cycle {
		return errors.New("remote identity sequence definition differs")
	}
	return nil
}

func (r *remoteTarget) verifyPrefix(ctx context.Context, tables []string, complete bool) error {
	order := remoteTableOrder()
	present := map[string]bool{}
	for _, name := range tables {
		present[name] = true
	}
	if complete && len(tables) != len(order) {
		return errors.New("remote target schema is incomplete")
	}
	for i, name := range order {
		if !present[name] {
			if i < len(tables) {
				return errors.New("remote schema is not a recognized creation prefix")
			}
			continue
		}
		if i >= len(tables) {
			return errors.New("remote schema is not a recognized creation prefix")
		}
		if err := r.verifyTable(ctx, name); err != nil {
			return err
		}
		n, err := r.missingIndexes(ctx, name)
		if err != nil {
			return err
		}
		if n != len(store.Schema[name].Indexes) && (complete || i != len(tables)-1) {
			return errors.New("remote schema has an incomplete earlier table")
		}
	}
	return nil
}

// sqlAtom removes only formatting and the known PostgreSQL casts emitted for
// our scalar defaults/checks. The remaining token stream must match exactly.
func sqlAtom(s string) string {
	var out strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			out.WriteByte(ch)
			if quoted && i+1 < len(s) && s[i+1] == '\'' {
				out.WriteByte(s[i+1])
				i++
				continue
			}
			quoted = !quoted
			continue
		}
		if quoted {
			out.WriteByte(ch)
			continue
		}
		skipped := false
		for _, cast := range []string{"::character varying", "::text[]", "::text"} {
			if strings.HasPrefix(s[i:], cast) {
				i += len(cast) - 1
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		if strings.ContainsRune("\"` \n\t()", rune(ch)) {
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func expectedDefault(c store.Column, dialect string) string {
	if c.ServerDefault == nil {
		return ""
	}
	d := *c.ServerDefault
	if c.Kind == "bool" {
		if d == "0" {
			if dialect == "postgres" {
				return "false"
			}
			return "0"
		}
		if dialect == "postgres" {
			return "true"
		}
		return "1"
	}
	if c.Kind != "integer" && c.Kind != "bigint" {
		return "'" + strings.ReplaceAll(d, "'", "''") + "'"
	}
	return d
}

func expectedType(c store.Column, dialect string) string {
	switch c.Kind {
	case "integer":
		if dialect == "mysql" {
			return "int"
		}
		return "integer"
	case "bigint":
		return "bigint"
	case "bool":
		if dialect == "mysql" {
			return "tinyint(1)"
		}
		return "boolean"
	case "string":
		if dialect == "mysql" {
			return fmt.Sprintf("varchar(%d)", c.Length)
		}
		return fmt.Sprintf("character varying(%d)", c.Length)
	case "datetime":
		if dialect == "mysql" {
			return "datetime(6)"
		}
		return "timestamp without time zone"
	case "decimal":
		if dialect == "mysql" {
			return fmt.Sprintf("decimal(%d,%d)", c.Precision, c.Scale)
		}
		return fmt.Sprintf("numeric(%d,%d)", c.Precision, c.Scale)
	case "text":
		if dialect == "mysql" && c.Medium {
			return "mediumtext"
		}
		return "text"
	}
	return "unsupported"
}

func (r *remoteTarget) verifyTable(ctx context.Context, name string) error {
	t := store.Schema[name]
	if r.store.Dialect == "mysql" {
		rows, err := r.queryer().QueryContext(ctx, `SELECT column_name,column_type,is_nullable,LEFT(COALESCE(column_default,''),4097),extra,COALESCE(character_set_name,''),COALESCE(collation_name,''),LEFT(COALESCE(generation_expression,''),4097) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position`, name)
		if err != nil {
			return errors.New("cannot inspect remote columns")
		}
		n := 0
		for rows.Next() {
			var column, typ, nullable, def, extra, charset, collation, generation string
			if rows.Scan(&column, &typ, &nullable, &def, &extra, &charset, &collation, &generation) != nil {
				rows.Close()
				return errors.New("cannot inspect remote columns")
			}
			if n >= len(t.Columns) {
				rows.Close()
				return errors.New("remote table contains extra columns")
			}
			c := t.Columns[n]
			n++
			// MariaDB displays integer widths; their storage domain is unchanged.
			if c.Kind == "integer" && typ == "int(11)" {
				typ = "int"
			}
			if c.Kind == "bigint" && typ == "bigint(20)" {
				typ = "bigint"
			}
			wantExtra := ""
			if c.Auto {
				wantExtra = "auto_increment"
			}
			if def == "NULL" && c.Nullable && c.ServerDefault == nil {
				def = ""
			}
			character := c.Kind == "string" || c.Kind == "text"
			if column != c.Name || typ != expectedType(c, "mysql") || (nullable == "YES") != c.Nullable || sqlAtom(def) != sqlAtom(expectedDefault(c, "mysql")) || extra != wantExtra || generation != "" || (character && (charset != "utf8mb4" || collation != "utf8mb4_unicode_ci")) || (!character && (charset != "" || collation != "")) {
				rows.Close()
				return fmt.Errorf("remote column definition differs for %s.%s", name, c.Name)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || n != len(t.Columns) {
			return errors.New("remote column inventory is incomplete")
		}
	} else {
		rows, err := r.queryer().QueryContext(ctx, `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attidentity::text,a.attgenerated::text,LEFT(COALESCE(pg_catalog.pg_get_expr(d.adbin,d.adrelid),''),4097),a.attisdropped,a.attcollation=t.typcollation,a.atthasmissing,a.attinhcount,a.attislocal FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_type t ON t.oid=a.atttypid LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE c.relnamespace=current_schema()::regnamespace AND c.relname=$1 AND a.attnum>0 ORDER BY a.attnum`, name)
		if err != nil {
			return errors.New("cannot inspect remote columns")
		}
		n := 0
		for rows.Next() {
			var column, typ, identity, generated, def string
			var notnull, dropped, defaultCollation, hasmissing, islocal bool
			var inherited int
			if rows.Scan(&column, &typ, &notnull, &identity, &generated, &def, &dropped, &defaultCollation, &hasmissing, &inherited, &islocal) != nil {
				rows.Close()
				return errors.New("cannot inspect remote columns")
			}
			if n >= len(t.Columns) {
				rows.Close()
				return errors.New("remote table contains extra columns")
			}
			c := t.Columns[n]
			n++
			wantIdentity := ""
			if c.Auto {
				wantIdentity = "d"
			}
			if column != c.Name || typ != expectedType(c, "postgres") || notnull == c.Nullable || identity != wantIdentity || generated != "" || sqlAtom(def) != sqlAtom(expectedDefault(c, "postgres")) || dropped || !defaultCollation || hasmissing || inherited != 0 || !islocal {
				rows.Close()
				return fmt.Errorf("remote column definition differs for %s.%s", name, c.Name)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || n != len(t.Columns) {
			return errors.New("remote column inventory is incomplete")
		}
	}
	return r.verifyConstraints(ctx, name)
}

func constraintSet(t store.Table) []string {
	out := []string{"p:" + strings.Join(t.PrimaryKey, "|")}
	for _, u := range t.Unique {
		out = append(out, "u:"+strings.Join(u, "|"))
	}
	for _, c := range t.Columns {
		if c.Reference != "" {
			out = append(out, "f:"+c.Name+":"+c.Reference+":"+strings.ToUpper(c.OnDelete))
		}
		if len(c.Enum) > 0 {
			out = append(out, "c:"+c.Name+":"+strings.Join(c.Enum, "|"))
		}
	}
	sort.Strings(out)
	return out
}

func (r *remoteTarget) verifyConstraints(ctx context.Context, name string) error {
	t := store.Schema[name]
	got := []string{}
	if r.store.Dialect == "postgres" {
		rows, err := r.queryer().QueryContext(ctx, `SELECT co.contype::text,
 COALESCE((SELECT string_agg(a.attname,'|' ORDER BY k.n) FROM unnest(co.conkey) WITH ORDINALITY k(attnum,n) JOIN pg_catalog.pg_attribute a ON a.attrelid=co.conrelid AND a.attnum=k.attnum),''),
 COALESCE(nt.nspname,''),COALESCE(ct.relname,''),
 COALESCE((SELECT string_agg(a.attname,'|' ORDER BY k.n) FROM unnest(co.confkey) WITH ORDINALITY k(attnum,n) JOIN pg_catalog.pg_attribute a ON a.attrelid=co.confrelid AND a.attnum=k.attnum),''),
 co.confdeltype::text,co.confupdtype::text,co.confmatchtype::text,co.convalidated,co.condeferrable,co.condeferred,co.connoinherit,co.coninhcount,co.conislocal,LEFT(COALESCE(pg_catalog.pg_get_expr(co.conbin,co.conrelid),''),4097)
 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class c ON c.oid=co.conrelid LEFT JOIN pg_catalog.pg_class ct ON ct.oid=co.confrelid LEFT JOIN pg_catalog.pg_namespace nt ON nt.oid=ct.relnamespace WHERE c.relnamespace=current_schema()::regnamespace AND c.relname=$1`, name)
		if err != nil {
			return remoteInspectionError(ctx, name, "postgres-constraints/query", err)
		}
		for rows.Next() {
			var kind, key, namespace, refTable, refKey, del, update, match, check string
			var valid, deferrable, deferred, noinherit, local bool
			var inherited int
			if err = rows.Scan(&kind, &key, &namespace, &refTable, &refKey, &del, &update, &match, &valid, &deferrable, &deferred, &noinherit, &inherited, &local, &check); err != nil {
				rows.Close()
				return remoteInspectionError(ctx, name, "postgres-constraints/scan", err)
			}
			if !valid || deferrable || deferred || (kind == "c" && noinherit) || inherited != 0 || !local {
				rows.Close()
				return errors.New("remote constraint enforcement differs")
			}
			switch kind {
			case "p", "u":
				got = append(got, kind+":"+key)
			case "f":
				action := map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[del]
				if namespace != r.namespace || update != "a" || match != "s" {
					rows.Close()
					return errors.New("remote foreign key definition differs")
				}
				got = append(got, "f:"+key+":"+refTable+"."+refKey+":"+action)
			case "c":
				c, ok := t.Column(key)
				if !ok || len(c.Enum) == 0 || sqlAtom(check) != sqlAtom(c.Name+" = ANY ARRAY["+quotedEnum(c.Enum)+"]") {
					rows.Close()
					return errors.New("remote check constraint definition differs")
				}
				got = append(got, "c:"+key+":"+strings.Join(c.Enum, "|"))
			default:
				rows.Close()
				return errors.New("remote constraint kind is unsupported")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return remoteInspectionError(ctx, name, "postgres-constraints/iterate", err)
		}
	} else {
		rows, err := r.queryer().QueryContext(ctx, `SELECT tc.constraint_name,tc.constraint_type,COALESCE(k.column_name,''),COALESCE(k.referenced_table_schema,''),COALESCE(k.referenced_table_name,''),COALESCE(k.referenced_column_name,''),COALESCE(rc.delete_rule,''),COALESCE(rc.update_rule,''),LEFT(COALESCE(cc.check_clause,''),4097) FROM information_schema.table_constraints tc LEFT JOIN information_schema.key_column_usage k ON k.constraint_schema=tc.constraint_schema AND k.table_name=tc.table_name AND k.constraint_name=tc.constraint_name LEFT JOIN information_schema.referential_constraints rc ON rc.constraint_schema=tc.constraint_schema AND rc.table_name=tc.table_name AND rc.constraint_name=tc.constraint_name LEFT JOIN information_schema.check_constraints cc ON cc.constraint_schema=tc.constraint_schema AND cc.table_name=tc.table_name AND cc.constraint_name=tc.constraint_name WHERE tc.table_schema=DATABASE() AND tc.table_name=? ORDER BY tc.constraint_name,k.ordinal_position`, name)
		if err != nil {
			return remoteInspectionError(ctx, name, "mysql-constraints/query", err)
		}
		keys := map[string][]string{}
		kinds := map[string]string{}
		for rows.Next() {
			var constraint, kind, key, namespace, refTable, refKey, del, update, check string
			if err = rows.Scan(&constraint, &kind, &key, &namespace, &refTable, &refKey, &del, &update, &check); err != nil {
				rows.Close()
				return remoteInspectionError(ctx, name, "mysql-constraints/scan", err)
			}
			switch kind {
			case "PRIMARY KEY", "UNIQUE":
				keys[constraint] = append(keys[constraint], key)
				kinds[constraint] = map[string]string{"PRIMARY KEY": "p", "UNIQUE": "u"}[kind]
			case "FOREIGN KEY":
				if namespace != r.namespace || (update != "RESTRICT" && update != "NO ACTION") {
					rows.Close()
					return errors.New("remote foreign key definition differs")
				}
				got = append(got, "f:"+key+":"+refTable+"."+refKey+":"+del)
			case "CHECK":
				found := false
				for _, c := range t.Columns {
					if len(c.Enum) > 0 && sqlAtom(check) == sqlAtom(c.Name+"in("+quotedEnum(c.Enum)+")") {
						got = append(got, "c:"+c.Name+":"+strings.Join(c.Enum, "|"))
						found = true
						break
					}
				}
				if !found {
					rows.Close()
					return errors.New("remote check constraint definition differs")
				}
			default:
				rows.Close()
				return errors.New("remote constraint kind is unsupported")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return remoteInspectionError(ctx, name, "mysql-constraints/iterate", err)
		}
		for constraint, key := range keys {
			got = append(got, kinds[constraint]+":"+strings.Join(key, "|"))
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, constraintSet(t)) {
		return fmt.Errorf("remote constraint set differs for %s", name)
	}
	return nil
}

func quotedEnum(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return strings.Join(out, ",")
}

type remoteIndex struct {
	name            string
	columns         []string
	lengths         []int
	unique, primary bool
}

func (r *remoteTarget) missingIndexes(ctx context.Context, name string) (int, error) {
	t := store.Schema[name]
	indexes := []remoteIndex{}
	if r.store.Dialect == "postgres" {
		rows, err := r.queryer().QueryContext(ctx, `SELECT x.relname,i.indisunique,i.indisprimary,
 (SELECT string_agg(LEFT(pg_catalog.pg_get_indexdef(i.indexrelid,k,true),256),'|' ORDER BY k) FROM generate_series(1,i.indnkeyatts) k),
 i.indisvalid AND i.indisready AND i.indislive AND NOT i.indisexclusion AND NOT i.indnullsnotdistinct AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indnkeyatts=i.indnatts AND am.amname='btree' AND
 NOT EXISTS(SELECT 1 FROM unnest(i.indoption) o WHERE o<>0) AND
 NOT EXISTS(SELECT 1 FROM unnest(i.indclass) o JOIN pg_catalog.pg_opclass op ON op.oid=o WHERE NOT op.opcdefault) AND
 NOT EXISTS(SELECT 1 FROM unnest(i.indkey::smallint[],i.indcollation::oid[]) k(attnum,collid) JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE a.attcollation<>k.collid)
 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class x ON x.oid=i.indexrelid JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_am am ON am.oid=x.relam WHERE c.relnamespace=current_schema()::regnamespace AND c.relname=$1 ORDER BY x.relname`, name)
		if err != nil {
			return 0, errors.New("cannot inspect remote indexes")
		}
		for rows.Next() {
			var idx remoteIndex
			var columns string
			var valid bool
			if rows.Scan(&idx.name, &idx.unique, &idx.primary, &columns, &valid) != nil || !valid {
				rows.Close()
				return 0, errors.New("remote index definition differs")
			}
			idx.columns = strings.Split(strings.ReplaceAll(columns, "\"", ""), "|")
			indexes = append(indexes, idx)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, errors.New("cannot inspect remote indexes")
		}
	} else {
		rows, err := r.queryer().QueryContext(ctx, `SELECT index_name,non_unique,seq_in_index,COALESCE(column_name,''),COALESCE(sub_part,0),COALESCE(collation,''),index_type,index_comment,comment,ignored FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? ORDER BY index_name,seq_in_index`, name)
		if err != nil {
			return 0, errors.New("cannot inspect remote index visibility (supported MariaDB metadata required)")
		}
		for rows.Next() {
			var index, column, collation, kind, indexComment, comment, ignored string
			var nonunique, seq, length int
			if rows.Scan(&index, &nonunique, &seq, &column, &length, &collation, &kind, &indexComment, &comment, &ignored) != nil || collation != "A" || kind != "BTREE" || indexComment != "" || comment != "" || ignored != "NO" {
				rows.Close()
				return 0, errors.New("remote index definition differs")
			}
			if len(indexes) == 0 || indexes[len(indexes)-1].name != index {
				indexes = append(indexes, remoteIndex{name: index, unique: nonunique == 0, primary: index == "PRIMARY"})
			}
			idx := &indexes[len(indexes)-1]
			if seq != len(idx.columns)+1 {
				rows.Close()
				return 0, errors.New("remote index column order differs")
			}
			idx.columns = append(idx.columns, column)
			idx.lengths = append(idx.lengths, length)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, errors.New("cannot inspect remote indexes")
		}
	}
	secondary := map[string]store.Index{}
	for _, idx := range t.Indexes {
		secondary[idx.Name] = idx
	}
	found := map[string]bool{}
	unique := []string{}
	primary := 0
	implicit := map[string]bool{}
	if r.store.Dialect == "mysql" {
		for _, c := range t.Columns {
			if c.Reference != "" {
				covered := len(t.PrimaryKey) > 0 && t.PrimaryKey[0] == c.Name
				for _, u := range t.Unique {
					if len(u) > 0 && u[0] == c.Name {
						covered = true
					}
				}
				// MySQL may remove an implicit FK index once an explicit index covers it.
				for _, idx := range indexes {
					if _, ok := secondary[idx.name]; ok && len(idx.columns) > 0 && idx.columns[0] == c.Name {
						covered = true
					}
				}
				if !covered {
					implicit[c.Name] = true
				}
			}
		}
	}
	for _, idx := range indexes {
		if idx.primary {
			if !idx.unique || !reflect.DeepEqual(idx.columns, t.PrimaryKey) || !zeroLengths(idx.lengths) {
				return 0, errors.New("remote primary index differs")
			}
			primary++
			continue
		}
		if idx.unique {
			if !zeroLengths(idx.lengths) {
				return 0, errors.New("remote unique index prefix differs")
			}
			unique = append(unique, strings.Join(idx.columns, "|"))
			continue
		}
		if expected, ok := secondary[idx.name]; ok {
			if !reflect.DeepEqual(idx.columns, expected.Columns) {
				return 0, errors.New("remote secondary index columns differ")
			}
			for _, length := range idx.lengths {
				if length != expected.MySQLLength {
					return 0, errors.New("remote secondary index prefix differs")
				}
			}
			found[idx.name] = true
			continue
		}
		if implicit[idx.name] && reflect.DeepEqual(idx.columns, []string{idx.name}) && zeroLengths(idx.lengths) {
			delete(implicit, idx.name)
			continue
		}
		return 0, errors.New("remote table contains an unexpected index")
	}
	expectedUnique := []string{}
	for _, u := range t.Unique {
		expectedUnique = append(expectedUnique, strings.Join(u, "|"))
	}
	sort.Strings(unique)
	sort.Strings(expectedUnique)
	if primary != 1 || !reflect.DeepEqual(unique, expectedUnique) || len(implicit) != 0 {
		return 0, errors.New("remote primary/unique/foreign index set differs")
	}
	n := 0
	for i, idx := range t.Indexes {
		if found[idx.Name] {
			if i != n {
				return 0, errors.New("remote secondary indexes are not a recognized prefix")
			}
			n++
		}
	}
	return n, nil
}

func zeroLengths(v []int) bool {
	for _, n := range v {
		if n != 0 {
			return false
		}
	}
	return true
}
