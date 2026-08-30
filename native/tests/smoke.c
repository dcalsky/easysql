#include "easysql.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void fail(const char *message, const char *response) {
    fprintf(stderr, "native smoke test failed: %s", message);
    if (response != NULL) {
        fprintf(stderr, ": %s", response);
    }
    fputc('\n', stderr);
    exit(1);
}

static char *execute(const char *request) {
    char *response = easysql_execute(request, strlen(request));
    if (response == NULL) {
        fail("easysql_execute returned NULL", NULL);
    }
    return response;
}

int main(void) {
    if (easysql_abi_version() != EASYSQL_ABI_VERSION) {
        fail("unexpected ABI version", NULL);
    }

    char *version = easysql_version();
    if (version == NULL || version[0] == '\0') {
        fail("empty library version", version);
    }
    easysql_free_string(version);

    const char *rewrite_request =
        "{\"abiVersion\":1,\"operation\":\"applyRowFilter\",\"args\":{"
        "\"sql\":\"SELECT id FROM orders\","
        "\"whereClause\":\"tenant_id = 7\",\"dialect\":\"postgres\"}}";

    for (int i = 0; i < 1000; i++) {
        char *response = execute(rewrite_request);
        if (strstr(response, "\"status\":0") == NULL ||
            strstr(response, "WHERE") == NULL ||
            strstr(response, "tenant_id") == NULL) {
            fail("unexpected rewrite response", response);
        }
        easysql_free_string(response);
    }

    char *invalid = execute("not-json");
    if (strstr(invalid, "\"status\":1") == NULL) {
        fail("malformed JSON was not rejected", invalid);
    }
    easysql_free_string(invalid);

    char *oversized = easysql_execute(rewrite_request, (size_t)(16 * 1024 * 1024 + 1));
    if (oversized == NULL || strstr(oversized, "\"status\":1") == NULL) {
        fail("oversized request was not rejected before copying", oversized);
    }
    easysql_free_string(oversized);

    easysql_free_string(NULL);

    const char *parse_error_request =
        "{\"abiVersion\":1,\"operation\":\"parseColumns\","
        "\"args\":{\"sql\":\"SELECT (\",\"dialect\":\"trino\"}}";
    char *parse_error = execute(parse_error_request);
    if (strstr(parse_error, "\"status\":2") == NULL) {
        fail("parse error was not classified", parse_error);
    }
    easysql_free_string(parse_error);

    puts("native C ABI smoke test OK");
    return 0;
}
