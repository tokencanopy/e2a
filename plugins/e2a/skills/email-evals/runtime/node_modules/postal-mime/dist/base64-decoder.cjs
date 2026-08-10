"use strict";
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);
var base64_decoder_exports = {};
__export(base64_decoder_exports, {
  default: () => Base64Decoder
});
module.exports = __toCommonJS(base64_decoder_exports);
var import_decode_strings = require("./decode-strings.cjs");
class Base64Decoder {
  constructor(opts) {
    opts = opts || {};
    this.decoder = opts.decoder || new TextDecoder();
    this.maxChunkSize = 100 * 1024;
    this.chunks = [];
    this.remainder = "";
  }
  update(buffer) {
    let str = this.decoder.decode(buffer);
    str = str.replace(/[^a-zA-Z0-9+\/]+/g, "");
    this.remainder += str;
    if (this.remainder.length >= this.maxChunkSize) {
      let allowedBytes = Math.floor(this.remainder.length / 4) * 4;
      let base64Str;
      if (allowedBytes === this.remainder.length) {
        base64Str = this.remainder;
        this.remainder = "";
      } else {
        base64Str = this.remainder.substr(0, allowedBytes);
        this.remainder = this.remainder.substr(allowedBytes);
      }
      if (base64Str.length) {
        this.chunks.push((0, import_decode_strings.decodeBase64)(base64Str));
      }
    }
  }
  finalize() {
    if (this.remainder && !/^=+$/.test(this.remainder)) {
      this.chunks.push((0, import_decode_strings.decodeBase64)(this.remainder));
    }
    return (0, import_decode_strings.blobToArrayBuffer)(new Blob(this.chunks, { type: "application/octet-stream" }));
  }
}

// Make default export work naturally with require()
if (module.exports.default) {
    var defaultExport = module.exports.default;
    var namedExports = {};
    for (var key in module.exports) {
        if (key !== 'default' && key !== '__esModule') {
            namedExports[key] = module.exports[key];
        }
    }
    module.exports = defaultExport;
    Object.assign(module.exports, namedExports);
    // Preserve __esModule and .default for bundler/transpiler interop
    Object.defineProperty(module.exports, '__esModule', { value: true });
    module.exports.default = defaultExport;
}

