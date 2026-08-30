#ifndef EASYSQL_H
#define EASYSQL_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32) && !defined(EASYSQL_STATIC)
#define EASYSQL_API __declspec(dllimport)
#else
#define EASYSQL_API
#endif

#ifdef __cplusplus
extern "C" {
#endif

#define EASYSQL_ABI_VERSION 1

#define EASYSQL_STATUS_SUCCESS 0
#define EASYSQL_STATUS_INVALID_ARGUMENT 1
#define EASYSQL_STATUS_PARSE_ERROR 2
#define EASYSQL_STATUS_UNSUPPORTED 3
#define EASYSQL_STATUS_INTERNAL_ERROR 4
#define EASYSQL_STATUS_PANIC 99

/* Return the stable ABI version. */
EASYSQL_API uint32_t easysql_abi_version(void);

/*
 * Return the native library version as a newly allocated UTF-8 string.
 * The caller must release it with easysql_free_string.
 */
EASYSQL_API char *easysql_version(void);

/*
 * Execute a versioned JSON request and return a newly allocated JSON response.
 * request_json does not need to be NUL terminated. The caller retains ownership
 * of the request buffer and must release the response with easysql_free_string.
 *
 * Every response contains abiVersion and status. On success it also contains
 * data; on failure it contains error. This function is thread-safe.
 */
EASYSQL_API char *easysql_execute(const void *request_json, size_t request_len);

/* Release a string returned by this library. NULL is accepted. */
EASYSQL_API void easysql_free_string(char *value);

#ifdef __cplusplus
}
#endif

#endif
