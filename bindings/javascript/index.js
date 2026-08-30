'use strict';

const path = require('node:path');
const koffi = require('koffi');

const ABI_VERSION = 1;

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

class EasySQL {
  constructor(libraryPath = defaultLibraryPath()) {
    this.library = koffi.load(libraryPath);
    this._abiVersion = this.library.func('uint32_t easysql_abi_version()');
    this._version = this.library.func('void *easysql_version()');
    this._execute = this.library.func('void *easysql_execute(const void *data, size_t length)');
    this._free = this.library.func('void easysql_free_string(void *value)');

    const actual = this._abiVersion();
    if (actual !== ABI_VERSION) {
      throw new Error(`easysql ABI ${actual} is incompatible with binding ABI ${ABI_VERSION}`);
    }
  }

  _consume(pointer) {
    if (!pointer) throw new Error('easysql returned a null response');
    try {
      return koffi.decode.string(pointer);
    } finally {
      this._free(pointer);
    }
  }

  version() {
    return this._consume(this._version());
  }

  execute(request) {
    const text = Buffer.isBuffer(request)
      ? request
      : Buffer.from(typeof request === 'string' ? request : JSON.stringify(request), 'utf8');
    return JSON.parse(this._consume(this._execute(text, text.length)));
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
