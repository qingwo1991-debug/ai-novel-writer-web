//! Synthetic-fixture-only native LanceDB/Arrow bridge. See include/novel_lance.h.
//! API references: github.com/lancedb/lancedb/tree/v0.22.3/rust/lancedb/src
//! and github.com/apache/arrow-rs/tree/56.2.0/arrow-ipc/src .

use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Cursor, Read, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::path::{Component, Path, PathBuf};
use std::ptr;
use std::slice;

use arrow_array::{Array, FixedSizeListArray, Float32Array, Float64Array, Int32Array,
    RecordBatch, RecordBatchReader, StringArray};
use arrow_ipc::reader::StreamReader;
use arrow_ipc::writer::StreamWriter;
use arrow_schema::{DataType, Field, SchemaRef};
use futures::TryStreamExt;
use lancedb::database::CreateTableMode;
use lancedb::query::{ExecutableQuery, QueryBase, Select};
use serde::{Deserialize, Serialize};
use tokio::runtime::Runtime;

const MAX_IPC: usize = 64 * 1024 * 1024;
const MAX_MANIFEST: usize = 1024 * 1024;
const MAX_TABLES: usize = 32;
const MAX_PATH: usize = 4096;
const MAX_TREE_ENTRIES: usize = 100_000;
const MAX_DOMAIN: usize = 64 * 1024 * 1024;
const MAX_DOMAIN_ROWS: usize = 10_000;
const MAX_DOMAIN_CELLS: usize = 200_000;
const MAX_DOMAIN_STRINGS: usize = 32 * 1024 * 1024;
const MAX_DOMAIN_FIELDS: usize = 128;
const MAX_DOMAIN_DEPTH: usize = 16;

#[derive(Clone, Copy)]
enum Fault {
    Input = 1,
    Path = 2,
    Limit = 3,
    Database = 4,
    Ipc = 5,
    Io = 6,
    Runtime = 7,
    Panic = 8,
}

impl Fault {
    fn token(self) -> &'static [u8] {
        match self {
            Self::Input => b"NV_LANCE_INPUT",
            Self::Path => b"NV_LANCE_PATH",
            Self::Limit => b"NV_LANCE_LIMIT",
            Self::Database => b"NV_LANCE_DATABASE",
            Self::Ipc => b"NV_LANCE_IPC",
            Self::Io => b"NV_LANCE_IO",
            Self::Runtime => b"NV_LANCE_RUNTIME",
            Self::Panic => b"NV_LANCE_PANIC",
        }
    }
}

type Result<T> = std::result::Result<T, Fault>;

#[repr(C)]
pub struct NvBytes {
    pub ptr: *mut u8,
    pub len: usize,
}

impl NvBytes {
    fn empty() -> Self {
        Self { ptr: ptr::null_mut(), len: 0 }
    }

    fn owned(value: Vec<u8>) -> Self {
        if value.is_empty() {
            return Self::empty();
        }
        let mut value = value.into_boxed_slice();
        let out = Self { ptr: value.as_mut_ptr(), len: value.len() };
        std::mem::forget(value);
        out
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Manifest {
    tables: Vec<TableEntry>,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct TableEntry {
    name: String,
    version: u64,
    rows: u64,
    file: String,
}

// This deliberately narrow sidecar is not an Arrow compatibility layer. It is
// derived from the SAME checked-out batches as IPC, never a second live query.
#[derive(Serialize)]
struct Domain {
    tables: Vec<DomainTable>,
}

#[derive(Serialize)]
struct DomainTable {
    name: String,
    version: u64,
    metadata: bool,
    schema: Vec<DomainField>,
    records: Vec<serde_json::Map<String, serde_json::Value>>,
}

#[derive(Serialize)]
struct DomainField {
    name: String,
    #[serde(rename = "type")]
    kind: &'static str,
    nullable: bool,
    #[serde(rename = "listSize")]
    list_size: i32,
    metadata: bool,
    children: Vec<DomainField>,
}

#[derive(Default)]
struct DomainBudget {
    rows: usize,
    cells: usize,
    strings: usize,
    fields: usize,
}

impl DomainBudget {
    fn string(&mut self, value: &str) -> Result<()> {
        self.strings = self.strings.checked_add(value.len()).ok_or(Fault::Limit)?;
        if self.strings > MAX_DOMAIN_STRINGS { return Err(Fault::Limit); }
        Ok(())
    }
}

fn domain_field(field: &Field, depth: usize, budget: &mut DomainBudget) -> Result<DomainField> {
    if depth > MAX_DOMAIN_DEPTH { return Err(Fault::Limit); }
    budget.fields += 1;
    if budget.fields > MAX_DOMAIN_FIELDS { return Err(Fault::Limit); }
    budget.string(field.name())?;
    let (kind, list_size, children) = match field.data_type() {
        DataType::Utf8 => ("utf8", 0, Vec::new()),
        DataType::Int32 => ("int32", 0, Vec::new()),
        DataType::Float32 => ("float32", 0, Vec::new()),
        DataType::Float64 => ("float64", 0, Vec::new()),
        DataType::FixedSizeList(child, size) if *size >= 0 =>
            ("fixed_size_list", *size, vec![domain_field(child, depth + 1, budget)?]),
        _ => return Err(Fault::Ipc),
    };
    Ok(DomainField {
        name: field.name().clone(), kind, nullable: field.is_nullable(), list_size,
        metadata: !field.metadata().is_empty(), children,
    })
}

fn domain_value(array: &dyn Array, row: usize, depth: usize,
    budget: &mut DomainBudget) -> Result<serde_json::Value> {
    if depth > MAX_DOMAIN_DEPTH { return Err(Fault::Limit); }
    budget.cells += 1;
    if budget.cells > MAX_DOMAIN_CELLS { return Err(Fault::Limit); }
    if array.is_null(row) { return Ok(serde_json::Value::Null); }
    match array.data_type() {
        DataType::Utf8 => {
            let array = array.as_any().downcast_ref::<StringArray>().ok_or(Fault::Ipc)?;
            let value = array.value(row);
            budget.string(value)?;
            Ok(serde_json::Value::String(value.to_owned()))
        }
        DataType::Int32 => {
            let array = array.as_any().downcast_ref::<Int32Array>().ok_or(Fault::Ipc)?;
            Ok(serde_json::Value::from(array.value(row)))
        }
        DataType::Float32 => {
            let array = array.as_any().downcast_ref::<Float32Array>().ok_or(Fault::Ipc)?;
            // Widen the actual float32 value, not its shortest f32 decimal form.
            let value = array.value(row) as f64;
            if !value.is_finite() { return Err(Fault::Ipc); }
            Ok(serde_json::Value::from(value))
        }
        DataType::Float64 => {
            let array = array.as_any().downcast_ref::<Float64Array>().ok_or(Fault::Ipc)?;
            let value = array.value(row);
            if !value.is_finite() { return Err(Fault::Ipc); }
            Ok(serde_json::Value::from(value))
        }
        DataType::FixedSizeList(_, _) => {
            let array = array.as_any().downcast_ref::<FixedSizeListArray>().ok_or(Fault::Ipc)?;
            let values = array.value(row);
            if values.len() > MAX_DOMAIN_CELLS.saturating_sub(budget.cells) {
                return Err(Fault::Limit);
            }
            let mut items = Vec::with_capacity(values.len());
            for index in 0..values.len() {
                items.push(domain_value(values.as_ref(), index, depth + 1, budget)?);
            }
            Ok(serde_json::Value::Array(items))
        }
        _ => Err(Fault::Ipc),
    }
}

fn domain_batch(batch: &RecordBatch, table: &mut DomainTable,
    budget: &mut DomainBudget) -> Result<()> {
    budget.rows = budget.rows.checked_add(batch.num_rows()).ok_or(Fault::Limit)?;
    if budget.rows > MAX_DOMAIN_ROWS { return Err(Fault::Limit); }
    for row in 0..batch.num_rows() {
        let mut record = serde_json::Map::new();
        for (column, field) in table.schema.iter().enumerate() {
            budget.string(&field.name)?;
            let value = domain_value(batch.column(column).as_ref(), row, 0, budget)?;
            if record.insert(field.name.clone(), value).is_some() { return Err(Fault::Ipc); }
        }
        table.records.push(record);
    }
    Ok(())
}

// Bound serialized bytes while writing; do not first allocate an unbounded Vec.
// This is NOT a bound on Arrow/native process RSS, nor a logical-hash encoding.
#[derive(Default)]
struct DomainWriter {
    bytes: Vec<u8>,
    exceeded: bool,
}

impl Write for DomainWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > MAX_DOMAIN.saturating_sub(self.bytes.len()) {
            self.exceeded = true;
            return Err(io::Error::new(io::ErrorKind::InvalidData, "NV_LANCE_LIMIT"));
        }
        // Grow in bounded chunks, avoiding both per-byte reallocations and an
        // unchecked geometric capacity beyond the raw serialized-byte cap.
        let needed = self.bytes.len() + bytes.len();
        if needed > self.bytes.capacity() {
            let capacity = needed.saturating_add(65535).min(MAX_DOMAIN) / 65536 * 65536;
            self.bytes.try_reserve_exact(capacity - self.bytes.len())
                .map_err(|_| io::Error::new(io::ErrorKind::Other, "NV_LANCE_LIMIT"))?;
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> { Ok(()) }
}

fn write_domain(out: &Path, domain: Domain) -> Result<()> {
    let mut writer = DomainWriter::default();
    let encoded = serde_json::to_writer(&mut writer, &domain);
    if writer.exceeded { return Err(Fault::Limit); }
    encoded.map_err(|_| Fault::Limit)?;
    let mut file = OpenOptions::new().write(true).create_new(true).mode(0o600)
        .custom_flags(libc::O_NOFOLLOW).open(out.join("domain.json")).map_err(|_| Fault::Io)?;
    file.set_permissions(fs::Permissions::from_mode(0o600)).map_err(|_| Fault::Io)?;
    file.write_all(&writer.bytes).map_err(|_| Fault::Io)?;
    file.sync_all().map_err(|_| Fault::Io)
}

fn allowed_name(name: &str) -> bool {
    if name == "chunks" || name == "documents" {
        return true;
    }
    match name.strip_prefix("chunks__space_") {
        Some(suffix) => !suffix.is_empty() && suffix.len() <= 20
            && suffix.bytes().all(|c| c.is_ascii_digit()),
        None => false,
    }
}

// Copies foreign input immediately; no foreign allocation enters an async task.
unsafe fn input_owned(data: *const u8, len: usize, max: usize) -> Result<Vec<u8>> {
    if len > max {
        return Err(Fault::Limit);
    }
    if data.is_null() || len == 0 {
        return Err(Fault::Input);
    }
    Ok(unsafe { slice::from_raw_parts(data, len) }.to_vec())
}

fn existing_private_dir(raw: Vec<u8>) -> Result<PathBuf> {
    let text = String::from_utf8(raw).map_err(|_| Fault::Path)?;
    if text.bytes().any(|c| matches!(c, 0 | b':' | b'\\' | b'?' | b'#' | b'%')) {
        return Err(Fault::Path);
    }
    let path = PathBuf::from(&text);
    if !path.is_absolute() || path == Path::new("/") {
        return Err(Fault::Path);
    }
    let mut current = PathBuf::new();
    for component in path.components() {
        match component {
            Component::RootDir | Component::Normal(_) => current.push(component.as_os_str()),
            _ => return Err(Fault::Path),
        }
        let meta = fs::symlink_metadata(&current).map_err(|_| Fault::Path)?;
        if !meta.is_dir() || meta.file_type().is_symlink() {
            return Err(Fault::Path);
        }
    }
    let canonical = fs::canonicalize(&path).map_err(|_| Fault::Path)?;
    // Compare the original bytes too: reject duplicate/trailing separators and '.' .
    if canonical.as_os_str() != std::ffi::OsStr::new(&text) {
        return Err(Fault::Path);
    }
    let meta = fs::symlink_metadata(&canonical).map_err(|_| Fault::Path)?;
    if meta.permissions().mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(Fault::Path);
    }
    Ok(canonical)
}

fn disjoint(a: &Path, b: &Path) -> Result<()> {
    if a.starts_with(b) || b.starts_with(a) {
        return Err(Fault::Path);
    }
    Ok(())
}

fn require_empty(path: &Path) -> Result<()> {
    let mut entries = fs::read_dir(path).map_err(|_| Fault::Path)?;
    match entries.next() {
        None => Ok(()),
        Some(Ok(_)) => Err(Fault::Path),
        Some(Err(_)) => Err(Fault::Io),
    }
}

// No symlink/special/hardlinked files in the supplied physical copy. This is NOT
// a sandbox for malicious Lance manifests or concurrent same-user writers.
fn check_tree(path: &Path, depth: usize, entries: &mut usize) -> Result<()> {
    if depth > 64 {
        return Err(Fault::Limit);
    }
    for entry in fs::read_dir(path).map_err(|_| Fault::Io)? {
        let entry = entry.map_err(|_| Fault::Io)?;
        *entries += 1;
        if *entries > MAX_TREE_ENTRIES {
            return Err(Fault::Limit);
        }
        let meta = fs::symlink_metadata(entry.path()).map_err(|_| Fault::Path)?;
        if meta.is_dir() {
            check_tree(&entry.path(), depth + 1, entries)?;
        } else if !meta.is_file() || meta.nlink() != 1 {
            return Err(Fault::Path);
        }
    }
    Ok(())
}

fn path_text(path: &Path) -> Result<&str> {
    path.to_str().ok_or(Fault::Path)
}

struct CappedWriter<'a> {
    file: &'a mut File,
    written: usize,
    exceeded: bool,
}

impl Write for CappedWriter<'_> {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > MAX_IPC.saturating_sub(self.written) {
            self.exceeded = true;
            return Err(io::Error::new(io::ErrorKind::InvalidData, "NV_LANCE_LIMIT"));
        }
        let count = self.file.write(bytes)?;
        self.written += count;
        Ok(count)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.file.flush()
    }
}

async fn export_one(table: lancedb::Table, name: String, out: &Path,
    budget: &mut DomainBudget) -> Result<(TableEntry, DomainTable)> {
    let version = table.version().await.map_err(|_| Fault::Database)?;
    table.checkout(version).await.map_err(|_| Fault::Database)?;
    let schema: SchemaRef = table.schema().await.map_err(|_| Fault::Database)?;
    let expected_rows = table.count_rows(None).await.map_err(|_| Fault::Database)? as u64;
    if expected_rows > MAX_DOMAIN_ROWS.saturating_sub(budget.rows) as u64 {
        return Err(Fault::Limit);
    }
    budget.string(&name)?;
    let mut fields = Vec::with_capacity(schema.fields().len().min(MAX_DOMAIN_FIELDS));
    let mut names = BTreeSet::new();
    for field in schema.fields() {
        if !names.insert(field.name()) { return Err(Fault::Ipc); }
        fields.push(domain_field(field, 0, budget)?);
    }
    let mut domain = DomainTable {
        name: name.clone(), version, metadata: !schema.metadata().is_empty(),
        schema: fields, records: Vec::new(),
    };
    let query = table.query().select(Select::All);
    let mut stream = query.execute().await.map_err(|_| Fault::Database)?;
    // Refuse unexpected projections; use the TABLE schema even for no batches.
    if stream.schema().fields() != schema.fields() {
        return Err(Fault::Ipc);
    }
    let filename = format!("{name}.arrows");
    let mut file = OpenOptions::new().write(true).create_new(true).mode(0o600)
        .custom_flags(libc::O_NOFOLLOW).open(out.join(&filename)).map_err(|_| Fault::Io)?;
    // mode() is filtered by umask; enforce the exact requested output permission.
    file.set_permissions(fs::Permissions::from_mode(0o600)).map_err(|_| Fault::Io)?;
    let mut capped = CappedWriter { file: &mut file, written: 0, exceeded: false };
    let write_result: Result<u64> = async {
        let mut writer = StreamWriter::try_new(&mut capped, schema.as_ref()).map_err(|_| Fault::Ipc)?;
        let mut rows = 0_u64;
        while let Some(batch) = stream.try_next().await.map_err(|_| Fault::Database)? {
            if batch.schema().fields() != schema.fields() {
                return Err(Fault::Ipc);
            }
            rows = rows.checked_add(batch.num_rows() as u64).ok_or(Fault::Limit)?;
            domain_batch(&batch, &mut domain, budget)?;
            writer.write(&batch).map_err(|_| Fault::Ipc)?;
        }
        writer.finish().map_err(|_| Fault::Ipc)?;
        Ok(rows)
    }.await;
    if capped.exceeded {
        return Err(Fault::Limit);
    }
    let rows = write_result?;
    if rows != expected_rows {
        return Err(Fault::Database);
    }
    file.sync_all().map_err(|_| Fault::Io)?;
    Ok((TableEntry { name, version, rows, file: filename }, domain))
}

async fn export_db(path: &Path, out: &Path) -> Result<Manifest> {
    // connect() could create an absent root: validate existence BEFORE invoking it.
    // Caller must exclude concurrent path replacement throughout this operation.
    let db = lancedb::connect(path_text(path)?).execute().await.map_err(|_| Fault::Database)?;
    let mut names = db.table_names().execute().await.map_err(|_| Fault::Database)?;
    if names.len() > MAX_TABLES {
        return Err(Fault::Limit);
    }
    names.sort();
    if names.iter().any(|name| !allowed_name(name)) || names.windows(2).any(|w| w[0] == w[1]) {
        return Err(Fault::Input);
    }
    // Validate every physical table before opening any of them; never create one.
    for name in &names {
        let table_path = path.join(format!("{name}.lance"));
        let meta = fs::symlink_metadata(&table_path).map_err(|_| Fault::Path)?;
        if !meta.is_dir() || meta.file_type().is_symlink() {
            return Err(Fault::Path);
        }
    }
    let mut tables = Vec::with_capacity(names.len());
    let mut domain_tables = Vec::with_capacity(names.len());
    let mut budget = DomainBudget::default();
    for name in names {
        let table = db.open_table(&name).execute().await.map_err(|_| Fault::Database)?;
        let (entry, domain) = export_one(table, name, out, &mut budget).await?;
        tables.push(entry);
        domain_tables.push(domain);
    }
    write_domain(out, Domain { tables: domain_tables })?;
    Ok(Manifest { tables })
}

fn validate_manifest(raw: &[u8]) -> Result<Manifest> {
    let mut manifest: Manifest = serde_json::from_slice(raw).map_err(|_| Fault::Input)?;
    if manifest.tables.len() > MAX_TABLES {
        return Err(Fault::Limit);
    }
    let mut names = BTreeSet::new();
    for entry in &manifest.tables {
        if !allowed_name(&entry.name) || entry.version == 0
            || entry.file != format!("{}.arrows", entry.name)
            || !names.insert(entry.name.clone()) {
            return Err(Fault::Input);
        }
    }
    manifest.tables.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(manifest)
}

fn load_ipc(dir: &Path, entry: &TableEntry) -> Result<Vec<u8>> {
    // Filename was already restricted to exactly <whitelisted-name>.arrows.
    let path = dir.join(&entry.file);
    let before = fs::symlink_metadata(&path).map_err(|_| Fault::Path)?;
    if !before.is_file() || before.nlink() != 1 {
        return Err(Fault::Path);
    }
    if before.len() > MAX_IPC as u64 {
        return Err(Fault::Limit);
    }
    let file = OpenOptions::new().read(true).custom_flags(libc::O_NOFOLLOW)
        .open(path).map_err(|_| Fault::Path)?;
    let opened = file.metadata().map_err(|_| Fault::Io)?;
    if !opened.is_file() || opened.dev() != before.dev() || opened.ino() != before.ino()
        || opened.nlink() != 1 {
        return Err(Fault::Path);
    }
    let mut bytes = Vec::new();
    file.take((MAX_IPC + 1) as u64).read_to_end(&mut bytes).map_err(|_| Fault::Io)?;
    if bytes.len() > MAX_IPC {
        return Err(Fault::Limit);
    }
    Ok(bytes)
}

// Validate complete IPC and row count before writing ANY staging table. Own bytes
// are then reparsed, rather than keeping every decoded Arrow batch in memory.
fn validate_ipc(bytes: &[u8], expected_rows: u64) -> Result<SchemaRef> {
    let mut reader = StreamReader::try_new(Cursor::new(bytes), None).map_err(|_| Fault::Ipc)?;
    let schema = reader.schema();
    let mut rows = 0_u64;
    for batch in reader.by_ref() {
        let batch = batch.map_err(|_| Fault::Ipc)?;
        rows = rows.checked_add(batch.num_rows() as u64).ok_or(Fault::Limit)?;
    }
    if reader.get_ref().position() != bytes.len() as u64 || rows != expected_rows {
        return Err(Fault::Ipc);
    }
    Ok(schema)
}

async fn import_db(staging: &Path, input: &Path, manifest: Manifest) -> Result<Manifest> {
    let mut inputs = Vec::with_capacity(manifest.tables.len());
    let mut total_bytes = 0_usize;
    for entry in manifest.tables {
        let bytes = load_ipc(input, &entry)?;
        total_bytes = total_bytes.checked_add(bytes.len()).ok_or(Fault::Limit)?;
        if total_bytes > 128 * 1024 * 1024 { return Err(Fault::Limit); }
        let schema = validate_ipc(&bytes, entry.rows)?;
        inputs.push((entry, bytes, schema));
    }
    require_empty(staging)?;
    let db = lancedb::connect(path_text(staging)?).execute().await.map_err(|_| Fault::Database)?;
    let mut tables = Vec::with_capacity(inputs.len());
    for (entry, bytes, schema) in inputs {
        let reader = StreamReader::try_new(Cursor::new(bytes), None).map_err(|_| Fault::Ipc)?;
        let reader: Box<dyn RecordBatchReader + Send> = Box::new(reader);
        let table = db.create_table(&entry.name, reader).mode(CreateTableMode::Create)
            .execute().await.map_err(|_| Fault::Database)?;
        let version = table.version().await.map_err(|_| Fault::Database)?;
        table.checkout(version).await.map_err(|_| Fault::Database)?;
        if table.schema().await.map_err(|_| Fault::Database)?.as_ref() != schema.as_ref() {
            return Err(Fault::Ipc);
        }
        let query = table.query().select(Select::All);
        let mut stream = query.execute().await.map_err(|_| Fault::Database)?;
        let mut rows = 0_u64;
        while let Some(batch) = stream.try_next().await.map_err(|_| Fault::Database)? {
            rows = rows.checked_add(batch.num_rows() as u64).ok_or(Fault::Limit)?;
        }
        if rows != entry.rows {
            return Err(Fault::Ipc);
        }
        tables.push(TableEntry { name: entry.name, version, rows, file: entry.file });
    }
    Ok(Manifest { tables })
}

fn manifest_bytes(manifest: Manifest) -> Result<Vec<u8>> {
    serde_json::to_vec(&manifest).map_err(|_| Fault::Input)
}

// Both the output initialization and work are behind catch_unwind. Publication
// happens only after all resources scoped to the operation have been dropped.
unsafe fn boundary<F>(result: *mut NvBytes, err: *mut NvBytes, work: F) -> i32
where F: FnOnce() -> Result<Vec<u8>> {
    let caught = catch_unwind(AssertUnwindSafe(|| {
        if !result.is_null() {
            unsafe { ptr::write(result, NvBytes::empty()) };
        }
        if !err.is_null() {
            unsafe { ptr::write(err, NvBytes::empty()) };
        }
        if result.is_null() || err.is_null() || result == err {
            return Fault::Input as i32;
        }
        match work() {
            Ok(value) => {
                unsafe { ptr::write(result, NvBytes::owned(value)) };
                0
            }
            Err(fault) => {
                unsafe { ptr::write(err, NvBytes::owned(fault.token().to_vec())) };
                fault as i32
            }
        }
    }));
    match caught {
        Ok(status) => status,
        Err(payload) => {
            // Do not allow an arbitrary panic-payload destructor to unwind into C.
            std::mem::forget(payload);
            let diagnostic = catch_unwind(AssertUnwindSafe(|| {
                if !err.is_null() && err != result {
                    unsafe { ptr::write(err, NvBytes::owned(Fault::Panic.token().to_vec())) };
                }
            }));
            if let Err(payload) = diagnostic {
                std::mem::forget(payload);
            }
            Fault::Panic as i32
        }
    }
}

/// # Safety
/// Foreign pointers must meet the contract in novel_lance.h.
#[no_mangle]
pub unsafe extern "C" fn nv_lance_export(
    path: *const u8, pathlen: usize, outdir: *const u8, outlen: usize,
    result: *mut NvBytes, err: *mut NvBytes,
) -> i32 {
    unsafe { boundary(result, err, || {
        let path = existing_private_dir(input_owned(path, pathlen, MAX_PATH)?)?;
        let out = existing_private_dir(input_owned(outdir, outlen, MAX_PATH)?)?;
        disjoint(&path, &out)?;
        require_empty(&out)?;
        check_tree(&path, 0, &mut 0)?;
        let runtime = Runtime::new().map_err(|_| Fault::Runtime)?;
        let manifest = runtime.block_on(export_db(&path, &out))?;
        manifest_bytes(manifest)
    }) }
}

/// # Safety
/// Foreign pointers must meet the contract in novel_lance.h.
#[no_mangle]
pub unsafe extern "C" fn nv_lance_import(
    staging: *const u8, staginglen: usize, inputdir: *const u8, inputlen: usize,
    manifest: *const u8, manifestlen: usize, result: *mut NvBytes, err: *mut NvBytes,
) -> i32 {
    unsafe { boundary(result, err, || {
        let staging = existing_private_dir(input_owned(staging, staginglen, MAX_PATH)?)?;
        let input = existing_private_dir(input_owned(inputdir, inputlen, MAX_PATH)?)?;
        disjoint(&staging, &input)?;
        require_empty(&staging)?;
        let raw = input_owned(manifest, manifestlen, MAX_MANIFEST)?;
        let manifest = validate_manifest(&raw)?;
        let runtime = Runtime::new().map_err(|_| Fault::Runtime)?;
        let manifest = runtime.block_on(import_db(&staging, &input, manifest))?;
        manifest_bytes(manifest)
    }) }
}

/// # Safety
/// bytes must be NULL or an unmodified, uniquely owned result from this library.
#[no_mangle]
pub unsafe extern "C" fn nv_lance_free(bytes: *mut NvBytes) {
    let caught = catch_unwind(AssertUnwindSafe(|| {
        if bytes.is_null() {
            return;
        }
        let owned = unsafe { ptr::replace(bytes, NvBytes::empty()) };
        if !owned.ptr.is_null() {
            let raw = ptr::slice_from_raw_parts_mut(owned.ptr, owned.len);
            unsafe { drop(Box::from_raw(raw)) };
        }
    }));
    if let Err(payload) = caught {
        std::mem::forget(payload);
    }
}
