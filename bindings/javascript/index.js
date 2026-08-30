'use strict';

const path = require('node:path');
const koffi = require('koffi');

const ABI_VERSION = 1;
const pinnedLibraries = [];

function libraryName() {
  switch (process.platform) {
    case 'darwin': return 'libeasysql.dylib';
    case 'win32': return 'easysql.dll';
    case 'linux': return 'libeasysql.so';
    default: throw new Error(`unsupported operating system: ${process.platform}`);
  }
}

function defaultLibraryPath() {
  return process.env.EASYSQL_LIBRARY_PATH || path.resolve(__dirname, '..', '..', 'lib', libraryName());
}

function loadLibrary(libraryPath) {
  const resolved = path.resolve(libraryPath);
  if (process.platform === 'win32') {
    // Go c-shared DLLs are process runtimes and must not be unloaded before
    // process termination. Koffi automatically unloads unreferenced libraries,
    // so retain an independent Windows loader reference for the process lifetime.
    const kernel32 = koffi.load('kernel32.dll');
    const loadLibraryW = kernel32.func('void *LoadLibraryW(str16 filename)');
    const handle = loadLibraryW(resolved);
    if (!handle) throw new Error(`failed to pin easysql library: ${resolved}`);
    pinnedLibraries.push({kernel32, handle});
  }
  return koffi.load(resolved);
}

class EasySQL {
  constructor(libraryPath = defaultLibraryPath()) {
    this.library = loadLibrary(libraryPath);
    this._abiVersion = this.library.func('uint32_t easysql_abi_version()');
    this._versionInto = this.library.func('size_t easysql_version_into(void *output, size_t capacity)');
    this._executeInto = this.library.func('size_t easysql_execute_into(const void *data, size_t length, void *output, size_t capacity)');

    const actual = this._abiVersion();
    if (actual !== ABI_VERSION) {
      throw new Error(`easysql ABI ${actual} is incompatible with binding ABI ${ABI_VERSION}`);
    }
  }

  version() {
    return this._readInto((output, capacity) => this._versionInto(output, capacity));
  }

  execute(request) {
    const text = Buffer.isBuffer(request)
      ? request
      : Buffer.from(typeof request === 'string' ? request : JSON.stringify(request), 'utf8');
    return JSON.parse(this._readInto((output, capacity) =>
      this._executeInto(text, text.length, output, capacity)));
  }

  _readInto(write) {
    const required = Number(write(null, 0));
    if (!Number.isSafeInteger(required) || required < 1) {
      throw new Error(`easysql returned an invalid buffer size: ${required}`);
    }
    const output = Buffer.alloc(required);
    const actual = Number(write(output, output.length));
    if (actual !== required) {
      throw new Error(`easysql response size changed from ${required} to ${actual}`);
    }
    return output.subarray(0, required - 1).toString('utf8');
  }
}

let sharedClient;

function client() {
  if (!sharedClient) sharedClient = new EasySQL();
  return sharedClient;
}

module.exports = {
  ABI_VERSION,
  EasySQL,
  client,
  execute: request => client().execute(request),
  version: () => client().version(),
};
