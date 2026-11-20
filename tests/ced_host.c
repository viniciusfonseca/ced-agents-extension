#include "extensions/manager.h"
#include "file_operations.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

static CedCommandContext context(void *user) {
    (void)user;
    return (CedCommandContext){0};
}

static void message(void *user, const char *text) {
    (void)user;
    fprintf(stderr, "CED: %s\n", text);
}

static void tick(CedExtensions *host, double deadline) {
    assert(ced_ext_now() < deadline);
    ced_extensions_tick(host);
    const struct timespec interval = {.tv_nsec = 5000000};
    nanosleep(&interval, NULL);
}

static void settle(CedExtensions *host) {
    double deadline = ced_ext_now() + 8;
    do { tick(host, deadline); } while (host->job.running || host->reload);
}

int main(int argc, char **argv) {
    assert(argc == 2);
    const char *extension_id = "viniciusfonseca.agents";
    const char *command_id = "viniciusfonseca.agents.open";
    char temp[] = "/tmp/ced-agents-host-XXXXXX";
    char root[CED_PATH], profile[CED_PATH], parent[CED_PATH], error[CED_ERROR] = {0};
    assert(mkdtemp(temp) && realpath(temp, root) && realpath("/tmp", parent));
    assert(ced_join(profile, sizeof profile, root, "profile"));
    CedCommands commands = {0};
    CedExtensionHost callbacks = {.context=context, .message=message};
    CedExtensions *host = ced_extensions_create(&commands, callbacks, profile, root);
    assert(host);
    settle(host);
    assert(host->count == 0 && !host->trusted);
    assert(ced_extensions_job(host, EXT_INSPECT, argv[1]));
    settle(host);
    assert(host->review_open);
    assert(ced_extensions_job(host, EXT_INSTALL, NULL));
    settle(host);
    assert(host->count == 1 && commands.count == 1);
    assert(host->instances[0].record.manifest.tab_count == 1);
    assert(host->instances[0].record.manifest.tabs[0].terminal);
    assert(!ced_extensions_tab_action(&host->instances[0], 0, "open", ""));
    assert(!ced_commands_execute(&commands, command_id, &(CedCommandContext){0}, error));
    assert(!host->instances[0].process);
    assert(ced_extensions_job(host, EXT_TRUST, NULL));
    settle(host);
    assert(host->trusted);
    assert(ced_extensions_tab_action(&host->instances[0], 0, "open", ""));
    double deadline = ced_ext_now() + 8;
    while (*host->instances[0].command) { tick(host, deadline); }
    assert(host->instances[0].state == EXT_ACTIVE && !*host->instances[0].error);
    assert(!*host->reveal_tab);
    assert(ced_commands_execute(&commands, command_id, &(CedCommandContext){0}, error));
    deadline = ced_ext_now() + 8;
    while (*host->instances[0].command) tick(host, deadline);
    assert(!strcmp(host->reveal_tab, "viniciusfonseca.agents.panel"));
    assert(!strcmp(ced_ext_json_string(host->instances[0].tabs[0].model, "heading"), "Agents"));
    cJSON *model = host->instances[0].tabs[0].model;
    assert(!strcmp(ced_ext_json_string(cJSON_GetArrayItem(cJSON_GetObjectItemCaseSensitive(model, "selectors"), 0),
                                      "selected"), "codex"));
    assert(ced_extensions_tab_action(&host->instances[0], 0, "harness", "claude"));
    deadline = ced_ext_now() + 8;
    while (*host->instances[0].command) tick(host, deadline);
    model = host->instances[0].tabs[0].model;
    assert(!*host->instances[0].error);
    assert(!strcmp(ced_ext_json_string(cJSON_GetArrayItem(cJSON_GetObjectItemCaseSensitive(model, "selectors"), 0),
                                      "selected"), "claude"));
    *host->reveal_tab = 0;
    assert(ced_extensions_tab_action(&host->instances[0], 0, "refresh", ""));
    deadline = ced_ext_now() + 8;
    while (*host->instances[0].command) tick(host, deadline);
    assert(!*host->reveal_tab && host->instances[0].tabs[0].model && !*host->instances[0].error);
    assert(ced_extensions_job(host, EXT_DISABLE, extension_id));
    settle(host);
    deadline = ced_ext_now() + 8;
    while (host->instances[0].process) { tick(host, deadline); }
    assert(!host->instances[0].record.enabled);
    assert(!host->instances[0].tabs[0].model);
    ced_extensions_destroy(host);
    ced_commands_free(&commands);
    assert(ced_rpc_memory_used() == 0);
    assert(ced_file_delete(parent, root, error));
    puts("OK: native CED install, trust, terminal tab reveal, model refresh and cleanup");
    return 0;
}
